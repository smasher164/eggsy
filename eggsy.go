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
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type File struct {
	Path string
	io.ReadCloser
}

type FileSet interface {
	At(i int) (File, error)
	Len() int
}

type Executor struct {
	Dockerfile []byte
	Files      FileSet
	Cmd        string
	Timeout    time.Duration

	Stdout io.Writer
	Stderr io.Writer

	cli *client.Client
}

type TimeoutError string

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
	tw.Write(e.Dockerfile)
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

var runtime string

func (e *Executor) runContainer(ctx context.Context, tag, cID string) (err error) {
	t := int(e.Timeout.Seconds())
	if e.Timeout < 0 {
		t = -1
	}
	_, err = e.cli.ContainerCreate(
		ctx, &container.Config{
			AttachStdout: true,
			AttachStderr: true,
			// TODO: is this correct quoting of a shell command?
			Cmd:         strslice.StrSlice{"sh", "-c", fmt.Sprintf("\"%q\"", e.Cmd)},
			Image:       tag,
			StopTimeout: &t,
		},
		&container.HostConfig{
			// "runsc" for gvisor
			Runtime: runtime,
		}, nil, cID)
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
	go stdcopy.StdCopy(e.Stdout, e.Stderr, muxRC)
	return nil
}

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
