package controlplane

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const scaleToZeroDisablePath = "/uk/libukp/scale_to_zero_disable"

// awakeGate owns one kernel disable request while any process activity holds it.
// It never resets the kernel counter, which may also belong to other workers.
type awakeGate struct {
	mu    sync.Mutex
	write func(string) error
	refs  int
}

func openAwakeGate(path string) (*awakeGate, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("scale-to-zero interface: %w", err)
	}
	return &awakeGate{write: func(command string) error {
		// This is a kernel interface, not a regular file: do not create or truncate.
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		n, err := io.WriteString(file, command)
		if err == nil && n != len(command) {
			err = io.ErrShortWrite
		}
		return errors.Join(err, file.Close())
	}}, nil
}

func (g *awakeGate) hold() (func() error, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refs == 0 && g.write != nil {
		if err := g.write("+"); err != nil {
			return nil, fmt.Errorf("prevent scale-to-zero: %w", err)
		}
	}
	g.refs++
	released := false
	return func() error {
		g.mu.Lock()
		defer g.mu.Unlock()
		if released {
			return nil
		}
		if g.refs == 1 && g.write != nil {
			if err := g.write("-"); err != nil {
				return fmt.Errorf("allow scale-to-zero: %w", err)
			}
		}
		g.refs--
		released = true
		return nil
	}, nil
}

var errServiceStopping = errors.New("deployment worker is stopping")

// workMu orders enqueue against the worker's final empty-queue check. The
// queue's hold is acquired before persistence and lasts through terminal fsync.
func (c *controlPlane) enqueue(spec buildSpec) (storedJob, error) {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	if c.stopping {
		return storedJob{}, errServiceStopping
	}
	if err := c.holdQueueLocked(); err != nil {
		c.failWorker(err)
		return storedJob{}, err
	}
	if spec.Metro == "" {
		spec.Metro = c.metro
	}
	job, err := c.store.submit(spec, spec.Metro)
	if err != nil && !c.store.pending() {
		if releaseErr := c.releaseQueueLocked(); releaseErr != nil {
			c.failWorker(releaseErr)
			return storedJob{}, errors.Join(err, releaseErr)
		}
	}
	return job, err
}

func (c *controlPlane) protectPending() error {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	if c.store.pending() {
		return c.holdQueueLocked()
	}
	return nil
}

func (c *controlPlane) claimProtected() (*storedJob, error) {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	if c.store.pending() {
		if err := c.holdQueueLocked(); err != nil {
			return nil, err
		}
	}
	job, err := c.store.claim()
	if err != nil {
		return nil, err
	}
	if job == nil {
		if err := c.releaseQueueLocked(); err != nil {
			return nil, err
		}
	}
	return job, nil
}

func (c *controlPlane) holdQueueLocked() error {
	if c.queueRelease != nil {
		return nil
	}
	release, err := c.awake.hold()
	if err != nil {
		return err
	}
	c.queueRelease = release
	return nil
}

func (c *controlPlane) releaseQueueLocked() error {
	if c.queueRelease == nil {
		return nil
	}
	if err := c.queueRelease(); err != nil {
		return err
	}
	c.queueRelease = nil
	return nil
}

func (c *controlPlane) stopQueue() error {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	c.stopping = true
	return c.releaseQueueLocked()
}

func (c *controlPlane) failWorker(err error) {
	select {
	case c.failures <- err:
	default:
	}
}
