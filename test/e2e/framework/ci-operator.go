// +build e2e

package framework

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	rand.Seed(time.Now().Unix())
}

// CiOperatorCommand exposes a ci-operator invocation to a test and
// ensures the following semantics:
//  - the command will get SIGINT 1 minutes before the test deadline
//  - the command will get SIGKILL 10 seconds before the test deadline
//  - unique hashes ensure unique test namespaces for concurrent runs
//  - artifacts will be persisted and jUnit will be mangled to not
//    pollute the owning test's jUnit output
type CiOperatorCommand struct {
	cmd         *exec.Cmd
	artifactDir string

	t *T

	testDone    <-chan struct{}
	cleanupDone chan<- struct{}
}

func (c *CiOperatorCommand) AddArgs(args ...string) {
	if c.cmd.Process != nil {
		c.t.Fatal("attempted to add args after ci-operator started")
	}
	c.cmd.Args = append(c.cmd.Args, args...)
}

func (c *CiOperatorCommand) AddEnv(env ...string) {
	if c.cmd.Process != nil {
		c.t.Fatal("attempted to add env after ci-operator started")
	}
	c.cmd.Env = append(c.cmd.Env, env...)
}

func (c *CiOperatorCommand) ArtifactDir() string {
	return c.artifactDir
}

// newCiOperatorCommand returns the basic ci-operator command and artifact dir. Add args and env as necessary.
func newCiOperatorCommand(t *T) CiOperatorCommand {
	t.Helper()
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		c, cancel := context.WithDeadline(ctx, deadline.Add(-10*time.Second))
		ctx = c
		t.Cleanup(cancel) // this does not really matter but govet is upset
	}
	var artifactDir string
	if dir, set := os.LookupEnv("ARTIFACT_DIR"); set {
		artifactDir = filepath.Join(dir, strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(t.Name()))
		if err := os.MkdirAll(artifactDir, 0755); err != nil {
			t.Fatalf("could not create artifact dir for ci-operator: %v", err)
		}
	} else {
		artifactDir = t.TempDir()
	}
	t.Cleanup(func() {
		if walkErr := filepath.Walk(artifactDir, func(path string, info os.FileInfo, err error) error {
			if info.IsDir() {
				return nil
			}
			// if we do not mangle these file-names, the jUnit spyglass lens
			// will show (sometimes expected) failures in these files from delegated
			// ci-operator runs in the overview, which is confusing
			if strings.HasPrefix(info.Name(), "junit") {
				if err := os.Rename(path, strings.ReplaceAll(path, "/junit", "/_junit")); err != nil {
					t.Logf("failed to mangle jUnit filename for %s: %v", path, err)
				}
			}
			return nil
		}); walkErr != nil {
			t.Errorf("failed to walk fixture tree for comparison: %v", walkErr)
		}
	})
	cmd := exec.CommandContext(ctx, "ci-operator",
		"--input-hash="+strconv.Itoa(rand.Int()), // we need unique namespaces
		"--artifact-dir="+artifactDir,
	)
	cmd.Env = append(cmd.Env, KubernetesClientEnv(t)...)
	return CiOperatorCommand{
		cmd:         cmd,
		artifactDir: artifactDir,
		t:           t,
	}
}

func (c *CiOperatorCommand) Run() ([]byte, error) {
	c.t.Logf("running: %v", c.cmd.Args)
	var b bytes.Buffer
	c.cmd.Stdout = &b
	c.cmd.Stderr = &b
	if err := c.cmd.Start(); err != nil {
		c.t.Fatalf("could not start ci-operator command: %v", err)
	}
	if deadline, ok := c.t.Deadline(); ok {
		go func() {
			defer func() {
				c.cleanupDone <- struct{}{}
			}()
			select {
			case <-c.testDone:
				// nothing to do
				return
			case <-time.After(time.Until(deadline.Add(-1 * time.Minute))):
				// the command context will send a SIGKILL, but we want an earlier SIGINT to allow
				// cleanup and artifact retrieval for sensible test output
				if err := c.cmd.Process.Signal(os.Interrupt); err != nil && !strings.Contains(err.Error(), "os: process already finished") { // why don't they export this ...
					c.t.Errorf("could not interrupt ci-operator: %v", err)
				}
			}
		}()
	} else {
		// we're not doing cleanup, so signal we're done anyway
		c.cleanupDone <- struct{}{}
	}
	// TODO(skuznets): stream this output?
	err := c.cmd.Wait()
	output := b.Bytes()
	c.t.Logf("ci-operator output:\n%v", string(output))
	return output, err
}
