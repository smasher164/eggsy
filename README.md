# eggsy

eggsy's goal is to execute a set of source files in a sandboxed docker container, i.e. it's job is to execute with gVisor (Dockerfile, file set, command, timeout).
The FileSet just has to be a list of paths and their io.ReadCloser's. It is copied into the container along with the provided Dockerfile.
Execute means that after the Dockerfile is run, the provided shell command is executed with a user-defined timeout.
The sandbox is [gVisor](https://github.com/google/gvisor), intended to isolate a process in a container from the host's kernel.

Example:
```
package main

import (
    "context"
    "io/ioutil"
    "log"
    "os"
    "strings"
    "time"
)

const dockerfile = `
FROM golang:1.10
COPY somefile.go .
`

const cmd = "go run somefile.go"

const file = `
package main
import (
    "fmt"
    "time"
)
func main() {
    time.Sleep(10 * time.Second)
    fmt.Println("Hello from the container")
}
`

type fslice []File

func (f fslice) At(i int) (File, error) { return f[i], nil }
func (f fslice) Len() int               { return len(f) }

func main() {
    files := fslice{File{
        Path:       "somefile.go",
        ReadCloser: ioutil.NopCloser(strings.NewReader(file)),
    }}

    e := &Executor{
        Dockerfile: []byte(dockerfile),
        Files:      files,
        Cmd:        cmd,
        Timeout:    3 * time.Second,
        Stdout:     os.Stdout,
        Stderr:     os.Stderr,
    }
    err := e.Execute(context.Background())
    if err != nil {
        log.Println(err)
        return
    }
}
```
Which should output the message similar to the following:
```
2018/08/14 23:42:15 process "go run somefile.go" in container eb06ed18d403e87e28382a8867e44b7a from image 98897596d97f38af229c2847c6287079 has timed out
```

Further work is required to programmatically configure access to the network and certain system calls.