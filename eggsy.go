// MIT License

// Copyright (c) 2018 Akhil Indurti

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package eggsy

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type (
	// Network is a network mode for a container. See the
	// constant definitions for descriptions of valid network modes.
	Network int

	// TimeoutError represents an error with a container
	// finishing its command execution, given its timeout.
	TimeoutError string

	// File associates a path with readable data, used in a FileSet
	// to create a build context for a container environment.
	File struct {
		Path string
		io.ReadCloser
	}

	// FileSet is a list of files used to create a build context
	// for a container environment.
	FileSet interface {
		At(i int) (File, error)
		Len() int
	}

	// Executor represents a non-reusable sandbox for executing a command.
	Executor struct {
		// Dockerfile is the Dockerfile used to construct the container.
		Dockerfile string

		// Files holds the set of files to be transferred into the build context.
		Files FileSet

		// Cmd is the shell command to execute inside the container.
		Cmd string

		// Timeout represents the timeout for the container to exit after
		// it has been spawned. A Timeout < 0 means there is no timeout.
		// If the timeout is reached before the container exits on its own,
		// Execute will return a TimeoutError.
		Timeout time.Duration

		// Seccomp is the security profile used to constrain system calls made
		// from the container to the Linux kernel. The default profile is
		// provided by docker.
		Seccomp string

		// Net is the network mode for the container. The default mode
		// is a bridge network.
		Net Network

		// Stdout and Stderr specify the container's standard output and standard error.
		//
		// If either is nil, output will be written to the null device.
		//
		// If Stdout == Stderr, at most one goroutine at a time will call Write.
		Stdout io.Writer
		Stderr io.Writer

		cli   *client.Client
		spath string
	}
)

type syncWriter struct {
	m sync.Mutex
	w io.Writer
}

func (s *syncWriter) Write(p []byte) (n int, err error) {
	s.m.Lock()
	defer s.m.Unlock()
	n, err = s.w.Write(p)
	return
}

const (
	NoTimeout time.Duration = -1

	SEDefault    = ""
	SEUnconfined = "unconfined"

	// NetBridge is the default network mode. No ports are exposed to the
	// outside world and other containers are only accessible via IP.
	NetBridge Network = 0

	// NetNone disables all network access in the container except to localhost.
	NetNone Network = 1
)

func (n Network) mode() container.NetworkMode {
	switch n {
	case 0:
		return "bridge"
	case 1:
		return "none"
	default:
		panic(fmt.Sprintf("(%v) doesn't have a corresponding network mode", n))
	}
}

func (t TimeoutError) Error() string { return string(t) }

func (e *Executor) makeBuildContext() (io.Reader, error) {
	var rb, buf bytes.Buffer
	tw := tar.NewWriter(&rb)
	n := e.Files.Len()
	for i := 0; i < n; i++ {
		f, err := e.Files.At(i)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		path := filepath.Clean(f.Path)
		if err != nil {
			return nil, err
		}
		buf.Reset()
		size, err := io.Copy(&buf, f)
		if err != nil {
			return nil, err
		}
		tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0666,
			Size: size,
		})
		io.Copy(tw, &buf)
	}
	tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0666,
		Size: int64(len(e.Dockerfile)),
	})
	tw.Write([]byte(e.Dockerfile))
	if e.Seccomp != SEDefault && e.Seccomp != SEUnconfined {
		e.spath = randN(8) + ".json"
		tw.WriteHeader(&tar.Header{
			Name: e.spath,
			Mode: 0666,
			Size: int64(len(e.Seccomp)),
		})
		tw.Write([]byte(e.Seccomp))
	}
	if e.Seccomp == SEUnconfined {
		e.spath = "unconfined.json"
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &rb, nil
}

func randN(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Executor) runContainer(ctx context.Context, tag, cID string) (err error) {
	t := int(e.Timeout.Seconds())
	if e.Timeout < 0 {
		t = -1
	}
	// gvisor
	hc := &container.HostConfig{
		NetworkMode: e.Net.mode(),
		Runtime:     "runsc",
	}
	if e.Seccomp != SEDefault {
		hc.SecurityOpt = []string{"seccomp=" + e.spath}
	}
	_, err = e.cli.ContainerCreate(
		ctx, &container.Config{
			AttachStdout: true,
			AttachStderr: true,
			// TODO: is this correct quoting of a shell command?
			Cmd:         strslice.StrSlice{"sh", "-c", fmt.Sprintf("\"%q\"", e.Cmd)},
			Image:       tag,
			StopTimeout: &t,
		}, hc, nil, cID)
	if err != nil {
		return err
	}
	err = e.cli.ContainerStart(ctx, cID, types.ContainerStartOptions{})
	if err != nil {
		e.cli.ContainerStop(ctx, cID, nil)
		return err
	}
	// demux output stream into stdout and stderr
	muxRC, err := e.cli.ContainerLogs(ctx, cID, types.ContainerLogsOptions{
		Follow:     true,
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return err
	}
	if e.Stdout == nil {
		e.Stdout = ioutil.Discard
	}
	if e.Stderr == nil {
		e.Stderr = ioutil.Discard
	}
	if e.Stdout == e.Stderr {
		e.Stdout = &syncWriter{w: e.Stdout}
		e.Stderr = e.Stdout
	}
	go stdcopy.StdCopy(e.Stdout, e.Stderr, muxRC)
	return nil
}

// Execute takes in a context, executes the Executor's command
// in a container, and waits for the container to exit. The timeout
// of the provided context is different from the timeout of the
// container. Execute will return a TimeoutError on a container timeout.
func (e *Executor) Execute(ctx context.Context) (err error) {
	bc, err := e.makeBuildContext()
	if err != nil {
		return err
	}
	if e.cli, err = client.NewClientWithOpts(client.FromEnv); err != nil {
		return err
	}
	// generate image and container IDs
	tag := randN(16)
	cID := randN(16)

	// Build image from Dockerfile in environment
	r, err := e.cli.ImageBuild(ctx, bc, types.ImageBuildOptions{Tags: []string{tag}})
	if err != nil {
		return err
	}
	io.Copy(ioutil.Discard, r.Body)
	defer e.cli.ImageRemove(ctx, tag, types.ImageRemoveOptions{Force: true})

	// Run container from image with cmd
	t0 := time.Now().Format(time.RFC3339Nano)
	err = e.runContainer(ctx, tag, cID)
	if err != nil {
		return err
	}
	e.cli.ContainerStop(ctx, cID, nil)
	cx, cancel := context.WithCancel(ctx)
	// Detect timeout
	cm, cer := e.cli.Events(cx, types.EventsOptions{
		Since: t0,
		Filters: filters.NewArgs(
			filters.KeyValuePair{"container", cID},
			filters.KeyValuePair{"image", tag},
			filters.KeyValuePair{"event", "die"},
		),
	})
	for {
		select {
		case m := <-cm:
			cancel()
			ec, err := strconv.Atoi(m.Actor.Attributes["exitCode"])
			if err != nil {
				return err
			}
			if ec == 137 {
				return TimeoutError(fmt.Sprintf("process %q in container %s from image %s has timed out", e.Cmd, cID, tag))
			}
			return nil
		case e := <-cer:
			cancel()
			return e
		}
	}
	return nil
}
