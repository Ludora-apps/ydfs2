package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gorilla/mux"
)

// compatMux keeps method-qualified routes available before Go 1.22.
type compatMux struct{ *mux.Router }

func newCompatMux() *compatMux { return &compatMux{mux.NewRouter().UseEncodedPath()} }
func (m *compatMux) Handle(pattern string, handler http.Handler) {
	method, path, _ := strings.Cut(pattern, " ")
	methods := []string{method}
	if method == http.MethodGet {
		methods = append(methods, http.MethodHead)
	}
	if path == "/" {
		m.PathPrefix(path).Methods(methods...).Handler(handler)
	} else {
		m.Path(path).Methods(methods...).Handler(handler)
	}
}
func (m *compatMux) HandleFunc(pattern string, handler http.HandlerFunc) { m.Handle(pattern, handler) }
func pathValue(r *http.Request, name string) string {
	value, _ := url.PathUnescape(mux.Vars(r)[name])
	return value
}

// groupCommand cancels the entire Docker CLI process group on Go 1.19.
// Captured output uses a temporary file so inherited pipes cannot hold Wait open.
type groupCommand struct {
	*exec.Cmd
	ctx     context.Context
	done    chan struct{}
	stopped chan struct{}
}

func (c *groupCommand) Start() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if err := c.Cmd.Start(); err != nil {
		return err
	}
	c.done, c.stopped = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(c.stopped)
		select {
		case <-c.ctx.Done():
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		case <-c.done:
		}
	}()
	return nil
}
func (c *groupCommand) Wait() error {
	err := c.Cmd.Wait()
	if c.done != nil {
		close(c.done)
		<-c.stopped
	}
	if err == nil {
		err = c.ctx.Err()
	}
	return err
}
func (c *groupCommand) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}
func (c *groupCommand) capture(combined bool) ([]byte, error) {
	if c.Stdout != nil || (combined && c.Stderr != nil) {
		return nil, fmt.Errorf("command output already set")
	}
	f, err := os.CreateTemp("", "ydfs-command-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	c.Stdout = f
	if combined {
		c.Stderr = f
	}
	err = c.Run()
	data, readErr := os.ReadFile(f.Name())
	if err == nil {
		err = readErr
	}
	return data, err
}
func (c *groupCommand) Output() ([]byte, error)         { return c.capture(false) }
func (c *groupCommand) CombinedOutput() ([]byte, error) { return c.capture(true) }
