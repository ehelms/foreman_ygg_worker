package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"git.sr.ht/~spc/go-log"
	"github.com/google/uuid"
	pb "github.com/redhatinsights/yggdrasil/protocol"
)

const preStartErrorExitCode = 254

func dispatch(ctx context.Context, d *pb.Data, s *jobStorage) {
	event, prs := d.GetMetadata()["event"]
	if !prs {
		log.Warnln("Message metadata does not contain event field, assuming 'start'")
		event = "start"
	}

	switch event {
	case "start":
		startScript(ctx, d, s)
	case "cancel":
		cancel(ctx, d, s)
	default:
		log.Errorf("Received unknown event '%v'", event)
	}
}

func startScript(ctx context.Context, d *pb.Data, s *jobStorage) {
	jobUUID, jobUUIDP := d.GetMetadata()["job_uuid"]
	if !jobUUIDP {
		jobUUID = uuid.New().String()
		log.Warnf("No job uuid found in job's metadata, will not be able to cancel this job, using autogenerated UUID %v", jobUUID)
	}

	log.Infof("Starting job %v", jobUUID)

	updates := make(chan V1Update)

	oa := NewUpdateAggregator(d.GetMetadata()["return_url"], d.GetMessageId())
	go oa.Aggregate(updates, &YggdrasilGrpc{})

	job := V1JobDefinition{}
	job.Script = string(d.GetContent())
	log.Tracef("running script : %#v", job.Script)

	effectiveUser, effectiveUserP := d.GetMetadata()["effective_user"]
	if effectiveUserP && effectiveUser != "" {
		job.EffectiveUser = &effectiveUser
	}

	scriptfile, err := os.CreateTemp("", "ygg_rex")
	if err != nil {
		reportStartError(fmt.Sprintf("failed to create script tmp file: %v", err), updates)
		return
	}
	defer os.Remove(scriptfile.Name())

	n2, err := scriptfile.Write([]byte(job.Script))
	if err != nil {
		reportStartError(fmt.Sprintf("failed to write script to tmp file: %v", err), updates)
		return
	}
	log.Debugf("script of %d bytes written in : %#v", n2, scriptfile.Name())

	err = scriptfile.Close()
	if err != nil {
		reportStartError(fmt.Sprintf("%v", err), updates)
		return
	}

	err = os.Chmod(scriptfile.Name(), 0700)
	if err != nil {
		reportStartError(fmt.Sprintf("%v", err), updates)
		return
	}

	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("cd ~; export HOME=\"$PWD\"; exec %v", scriptfile.Name()))
	// cmd.Env = env
	if job.EffectiveUser != nil {
		u, err := user.Lookup(*job.EffectiveUser)
		if err != nil {
			reportStartError(fmt.Sprintf("Unknown effective user '%v'", *job.EffectiveUser), updates)
			return
		}
		uid, _ := strconv.ParseInt(u.Uid, 10, 32)
		gid, _ := strconv.ParseInt(u.Gid, 10, 32)

		err = os.Chown(scriptfile.Name(), int(uid), int(gid))
		if err != nil {
			reportStartError(fmt.Sprintf("Failed to change ownership of script: %v", err), updates)
			return
		}

		cmd.SysProcAttr = &syscall.SysProcAttr{}
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		reportStartError(fmt.Sprintf("cannot connect to stdout: %v", err), updates)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		reportStartError(fmt.Sprintf("cannot connect to stderr: %v", err), updates)
		return
	}

	if err := cmd.Start(); err != nil {
		reportStartError(fmt.Sprintf("cannot run script: %v", err), updates)
		return
	}

	log.Infof("started script process: %v", cmd.Process.Pid)
	if jobUUIDP {
		s.Set(jobUUID, cmd.Process.Pid)
		defer s.Remove(jobUUID)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() { outputCollector("stdout", stdout, updates); wg.Done() }()
	go func() { outputCollector("stderr", stderr, updates); wg.Done() }()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			// The program has exited with an exit code != 0
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				updates <- NewExitUpdate(status.ExitStatus())
			}
		} else {
			reportStartError(fmt.Sprintf("script run failed: %v", err), updates)
			return
		}
	} else {
		updates <- NewExitUpdate(0)
	}
	close(updates)
	log.Infof("Finished job %v", jobUUID)
}

func reportStartError(message string, updates chan<- V1Update) {
	log.Error(message)
	updates <- NewOutputUpdate("stderr", message)
	updates <- NewExitUpdate(preStartErrorExitCode)
	close(updates)
}

func outputCollector(stdtype string, pipe io.ReadCloser, outputs chan<- V1Update) {
	buf := make([]byte, 4096)
	for {
		n, err := pipe.Read(buf)
		if n > 0 {
			msg := string(buf[:n])
			log.Tracef("%v message: %v", stdtype, msg)
			outputs <- NewOutputUpdate(stdtype, msg)
		}
		if err != nil {
			if err != io.EOF {
				log.Errorf("cannot read from %v: %v", stdtype, err)
			}
			break
		}
	}
}

var syscallKill = syscall.Kill

func cancel(ctx context.Context, d *pb.Data, s *jobStorage) {
	jobUUID, jobUUIDP := d.GetMetadata()["job_uuid"]
	if !jobUUIDP {
		log.Errorln("No job uuid found in job's metadata, aborting.")
		return
	}

	pid, prs := s.Get(jobUUID)
	if !prs {
		log.Errorf("Cannot cancel unknown job %v", jobUUID)
		return
	}

	log.Infof("Cancelling job %v, sending SIGTERM to process %v", jobUUID, pid)
	if err := syscallKill(pid, syscall.SIGTERM); err != nil {
		log.Errorf("Failed to send SIGTERM to process %v: %v", pid, err)
	}
}
