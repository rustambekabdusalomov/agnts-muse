package muse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/rustambekabdusalomov/agnts-muse/credential"
)

// Command is a process to start: `muse serve` and what it needs.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
	// Files must exist before the process starts (a login's credential
	// file); the executor removes them when the process ends.
	Files []credential.File
}

// Process is a started `muse serve`: reads are its stdout, writes go to
// its stdin, Close closes stdin.
type Process interface {
	io.ReadWriteCloser
	// Wait waits for the process to exit.
	Wait() error
	// Kill stops it at once.
	Kill() error
}

// Executor starts processes. LocalExecutor runs them on this machine; a
// host that runs agents in containers implements Executor with
// `docker exec -i` (agnts does).
type Executor interface {
	Start(ctx context.Context, cmd Command) (Process, error)
}

// LocalExecutor runs Muse on this machine.
type LocalExecutor struct {
	// Stderr receives the process's stderr (discarded when nil).
	Stderr io.Writer
	// InheritEnv passes this process's environment through, before
	// Command.Env. Off by default so a stray META_API_KEY cannot take
	// precedence over the credential a run was given.
	InheritEnv bool
}

// Start implements Executor.
func (l LocalExecutor) Start(ctx context.Context, cmd Command) (Process, error) {
	for _, f := range cmd.Files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(f.Path, f.Data, os.FileMode(f.Mode)); err != nil {
			return nil, err
		}
	}
	c := exec.Command(cmd.Path, cmd.Args...) //nolint:gosec // the caller chooses the binary
	c.Dir = cmd.Dir
	c.Env = cmd.Env
	if l.InheritEnv {
		c.Env = append(os.Environ(), cmd.Env...)
	} else {
		c.Env = append([]string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}, cmd.Env...)
	}
	c.Stderr = l.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Kill takes the tool processes Muse started too
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		removeFiles(cmd.Files)
		return nil, fmt.Errorf("muse: start %s: %w", cmd.Path, err)
	}
	p := &localProcess{cmd: c, stdin: stdin, stdout: stdout, files: cmd.Files, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

type localProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	files  []credential.File

	once sync.Once
	done chan struct{} // closed when Wait returns
	err  error
}

func (p *localProcess) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *localProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *localProcess) Close() error                { return p.stdin.Close() }

func (p *localProcess) Wait() error {
	p.once.Do(func() {
		p.err = p.cmd.Wait()
		removeFiles(p.files)
		close(p.done)
	})
	<-p.done
	return p.err
}

func (p *localProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func removeFiles(files []credential.File) {
	for _, f := range files {
		_ = os.Remove(f.Path)
	}
}
