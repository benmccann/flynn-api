package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/flynn/go-discover/discover"
	"github.com/flynn/lorne/types"
	sampic "github.com/flynn/sampi/client"
	"github.com/flynn/sampi/types"
	"github.com/rcrowley/go-tigertonic"
	"github.com/titanous/go-dockerclient"
)

func main() {
	var err error
	scheduler, err = sampic.New()
	if err != nil {
		log.Fatal(err)
	}
	disc, err = discover.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	mux := tigertonic.NewTrieServeMux()
	mux.Handle("POST", "/apps/{app_id}/formations/{formation_id}", tigertonic.Marshaled(changeFormation))
	mux.HandleFunc("GET", "/apps/{app_id}/jobs/{job_id}/logs", getJobLog)
	mux.HandleFunc("POST", "/apps/{app_id}/jobs", runJob)
	logger = tigertonic.Logged(mux, nil)
	http.ListenAndServe("127.0.0.1:1200", logger)
}

var logger *tigertonic.Logger
var scheduler *sampic.Client
var disc *discover.Client

type Job struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// GET /apps/{app_id}/jobs
func getJobList(u *url.URL, h http.Header) (int, http.Header, []Job, error) {
	state, err := scheduler.State()
	if err != nil {
		return 500, nil, nil, err
	}

	q := u.Query()
	prefix := q.Get("app_id") + "-"
	jobs := make([]Job, 0)
	for _, host := range state {
		for _, job := range host.Jobs {
			if strings.HasPrefix(job.ID, prefix) {
				typ := strings.Split(job.ID[len(prefix):], ".")[0]
				jobs = append(jobs, Job{ID: job.ID, Type: typ})
			}
		}
	}

	return 200, nil, jobs, nil
}

type Formation struct {
	Quantity int    `json:"quantity"`
	Type     string `json:"type"`
}

// POST /apps/{app_id}/formations/{formation_id}
func changeFormation(u *url.URL, h http.Header, req *Formation) (int, http.Header, *Formation, error) {
	state, err := scheduler.State()
	if err != nil {
		return 500, nil, nil, err
	}

	q := u.Query()
	prefix := q.Get("app_id") + "-" + q.Get("formation_id") + "."
	var jobs []*sampi.Job
	for _, host := range state {
		for _, job := range host.Jobs {
			if strings.HasPrefix(job.ID, prefix) {
				if job.Attributes == nil {
					job.Attributes = make(map[string]string)
				}
				job.Attributes["host_id"] = host.ID
				jobs = append(jobs, job)
			}
		}
	}

	if req.Quantity < 0 {
		req.Quantity = 0
	}
	diff := req.Quantity - len(jobs)
	if diff > 0 {
		config := &docker.Config{
			Image:        "titanous/redis",
			Cmd:          []string{"/bin/cat"},
			AttachStdout: true,
			AttachStderr: true,
		}
		schedReq := &sampi.ScheduleReq{
			HostJobs: make(map[string][]*sampi.Job),
		}
	outer:
		for {
			for host := range state {
				schedReq.HostJobs[host] = append(schedReq.HostJobs[host], &sampi.Job{ID: prefix + randomID(), Config: config})
				diff--
				if diff == 0 {
					break outer
				}
			}
		}

		res, err := scheduler.Schedule(schedReq)
		if err != nil || !res.Success {
			return 500, nil, nil, err
		}
	} else if diff < 0 {
		for _, job := range jobs[:-diff] {
			_ = job
			// connect to host service
			// stop job
		}
	}

	return 200, nil, req, nil
}

// GET /apps/{app_id}/jobs/{job_id}/logs
func getJobLog(w http.ResponseWriter, req *http.Request) {
	// get scheduler state
	// find job host
	// connect to host
	// fetch logs from specified job
}

type NewJob struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env"`
	Attach  bool              `json:"attach"`
	TTY     bool              `json:"tty"`
	Columns int               `json:"tty_columns"`
	Lines   int               `json:"tty_lines"`
}

// POST /apps/{app_id}/jobs
func runJob(w http.ResponseWriter, req *http.Request) {
	var jobReq NewJob
	if err := json.NewDecoder(req.Body).Decode(&jobReq); err != nil {
		w.WriteHeader(500)
		logger.Println(err)
		return
	}

	state, err := scheduler.State()
	if err != nil {
		w.WriteHeader(500)
		logger.Println(err)
		return
	}
	// pick a random host
	var hostID string
	for hostID = range state {
		break
	}
	if hostID == "" {
		w.WriteHeader(500)
		logger.Println("no hosts found")
		return
	}

	env := make([]string, 0, len(jobReq.Env))
	for k, v := range jobReq.Env {
		env = append(env, k+"="+v)
	}

	q := req.URL.Query()
	job := &sampi.Job{
		ID: q.Get("app_id") + "-run." + randomID(),
		Config: &docker.Config{
			Image:        "ubuntu",
			Cmd:          jobReq.Cmd,
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			StdinOnce:    true,
			Env:          env,
		},
	}
	if jobReq.TTY {
		job.Config.Tty = true
	}
	if jobReq.Attach {
		job.Config.AttachStdin = true
		job.Config.StdinOnce = true
		job.Config.OpenStdin = true
	}

	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	defer outR.Close()
	defer inW.Close()
	var errChan <-chan error
	if jobReq.Attach {
		attachReq := &lorne.AttachReq{
			JobID:  job.ID,
			Flags:  lorne.AttachFlagStdout | lorne.AttachFlagStderr | lorne.AttachFlagStdin | lorne.AttachFlagStream,
			Height: 0,
			Width:  0,
		}
		err, errChan = lorneAttach(hostID, attachReq, outW, inR)
		if err != nil {
			w.WriteHeader(500)
			logger.Println("attach failed", err)
			return
		}
	}

	res, err := scheduler.Schedule(&sampi.ScheduleReq{HostJobs: map[string][]*sampi.Job{hostID: {job}}})
	if err != nil || !res.Success {
		w.WriteHeader(500)
		logger.Println("schedule failed", err)
		return
	}

	if jobReq.Attach {
		w.Header().Set("Content-Type", "application/vnd.flynn.hijack")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
		conn, bufrw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		bufrw.Flush()
		go func() {
			buf := make([]byte, bufrw.Reader.Buffered())
			bufrw.Read(buf)
			inW.Write(buf)
			io.Copy(inW, conn)
			inW.Close()
		}()
		go io.Copy(conn, outR)
		<-errChan
		conn.Close()
		return
	}
	w.WriteHeader(200)
}

func lorneAttach(host string, req *lorne.AttachReq, out io.Writer, in io.Reader) (error, <-chan error) {
	services, err := disc.Services("flynn-lorne-attach." + host)
	if err != nil {
		return err, nil
	}
	addrs := services.OnlineAddrs()
	if len(addrs) == 0 {
		return err, nil
	}
	conn, err := net.Dial("tcp", addrs[0])
	if err != nil {
		return err, nil
	}
	err = gob.NewEncoder(conn).Encode(req)
	if err != nil {
		conn.Close()
		return err, nil
	}

	errChan := make(chan error)

	attach := func() {
		defer conn.Close()
		inErr := make(chan error, 1)
		if in != nil {
			go func() {
				io.Copy(conn, in)
			}()
		} else {
			close(inErr)
		}
		_, outErr := io.Copy(out, conn)
		if outErr != nil {
			errChan <- outErr
			return
		}
		errChan <- <-inErr
	}

	attachState := make([]byte, 1)
	if _, err := conn.Read(attachState); err != nil {
		conn.Close()
		return err, nil
	}
	switch attachState[0] {
	case lorne.AttachError:
		errBytes, err := ioutil.ReadAll(conn)
		conn.Close()
		if err != nil {
			return err, nil
		}
		return errors.New(string(errBytes)), nil
	case lorne.AttachWaiting:
		go func() {
			if _, err := conn.Read(attachState); err != nil {
				conn.Close()
				errChan <- err
				return
			}
			if attachState[0] == lorne.AttachError {
				errBytes, err := ioutil.ReadAll(conn)
				conn.Close()
				if err != nil {
					errChan <- err
					return
				}
				errChan <- errors.New(string(errBytes))
				return
			}
			attach()
		}()
		return nil, errChan
	default:
		go attach()
		return nil, errChan
	}
}

func randomID() string {
	b := make([]byte, 16)
	enc := make([]byte, 24)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		panic(err) // This shouldn't ever happen, right?
	}
	base64.URLEncoding.Encode(enc, b)
	return string(bytes.TrimRight(enc, "="))
}