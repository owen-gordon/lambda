package controlplane

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

var errQueueFull = errors.New("build queue is full")
var errJobLimit = errors.New("job storage limit reached; delete completed job records")
var errJobNotFound = errors.New("job not found")

// Job is the public view. Source, environment, and authentication credentials
// only live in the encrypted storedJob input, not this API representation.
type Job struct {
	ID         string       `json:"id"`
	Status     string       `json:"status"`
	Phase      string       `json:"phase"`
	Name       string       `json:"name"`
	Metro      string       `json:"metro"`
	Protected  bool         `json:"protected"`
	CreatedAt  time.Time    `json:"created_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	DeletedAt  *time.Time   `json:"deleted_at,omitempty"`
	Deployment *deployment  `json:"deployment,omitempty"`
	Error      string       `json:"error,omitempty"`
	Health     *healthCheck `json:"health,omitempty"`
}

type storedJob struct {
	Job  Job       `json:"job"`
	Spec buildSpec `json:"spec"`
}

type jobStore struct {
	mu         sync.Mutex
	directory  string
	aead       cipher.AEAD
	lock       *os.File
	jobs       map[string]storedJob
	maxPending int
	maxJobs    int
}

func openJobStore(directory string, key []byte, maxPending, maxJobs int) (*jobStore, error) {
	if len(key) != 32 {
		return nil, errors.New("state encryption key must contain 32 bytes")
	}
	if maxPending <= 0 || maxJobs < maxPending {
		return nil, errors.New("max-jobs must be at least max-pending, and both must be positive")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("state directory is already in use by another server")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		lock.Close()
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		lock.Close()
		return nil, err
	}
	store := &jobStore{directory: directory, aead: aead, lock: lock, jobs: make(map[string]storedJob), maxPending: maxPending, maxJobs: maxJobs}
	if err := store.load(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *jobStore) Close() error { return s.lock.Close() }

func (s *jobStore) load() error {
	// Verify the key even when all job records have been removed.
	checkPath := filepath.Join(s.directory, "keycheck")
	check, err := os.ReadFile(checkPath)
	if os.IsNotExist(err) {
		if err := s.atomicWrite(checkPath, s.aead.Seal(nil, nil, []byte("lambda-state-v1"), []byte("keycheck"))); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if plain, err := s.aead.Open(nil, nil, check, []byte("keycheck")); err != nil || string(plain) != "lambda-state-v1" {
		return errors.New("cannot decrypt state: check LAMBDA_STATE_KEY")
	}
	paths, err := filepath.Glob(filepath.Join(s.directory, "*.job"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		id := filepath.Base(path)
		id = id[:len(id)-len(".job")]
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid job filename %s", filepath.Base(path))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		plain, err := s.aead.Open(nil, nil, data, []byte(id))
		if err != nil {
			return fmt.Errorf("cannot decrypt job %s", id)
		}
		var job storedJob
		if err := json.Unmarshal(plain, &job); err != nil {
			return fmt.Errorf("invalid job %s", id)
		}
		if job.Job.ID != id {
			return errors.New("job filename does not match its record")
		}
		if job.Job.Status == "running" {
			job.Job.Status, job.Job.Phase = "interrupted", "interrupted"
			job.Job.Error = "worker restarted during deployment; inspect the instance before retrying"
			job.Job.FinishedAt = new(time.Now().UTC())
			if err := s.persist(job); err != nil {
				return err
			}
		}
		s.jobs[id] = job
	}
	return nil
}

func (s *jobStore) persist(job storedJob) error {
	plain, err := json.Marshal(job)
	if err != nil {
		return err
	}
	data := s.aead.Seal(nil, nil, plain, []byte(job.Job.ID))
	return s.atomicWrite(filepath.Join(s.directory, job.Job.ID+".job"), data)
}

func (s *jobStore) atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(s.directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(s.directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cloneJob(job storedJob) storedJob {
	// Clone nested maps and pointers so handlers cannot race the worker by
	// mutating shared records after releasing the store lock.
	data, _ := json.Marshal(job)
	var copy storedJob
	_ = json.Unmarshal(data, &copy)
	return copy
}

func (s *jobStore) submit(spec buildSpec, metro string) (storedJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) >= s.maxJobs {
		return storedJob{}, errJobLimit
	}
	pending := 0
	for _, job := range s.jobs {
		if job.Job.Status == "queued" || job.Job.Status == "running" {
			pending++
		}
	}
	if pending >= s.maxPending {
		return storedJob{}, errQueueFull
	}
	id := uuid.NewString()
	name := spec.Name
	if name == "" {
		name = "fn"
	}
	name += "-" + id[:12]
	job := storedJob{Spec: spec, Job: Job{ID: id, Status: "queued", Phase: "queued", Name: name, Metro: metro, Protected: spec.Auth.Mode == "bearer", CreatedAt: time.Now().UTC()}}
	if err := s.persist(job); err != nil {
		return storedJob{}, err
	}
	s.jobs[id] = cloneJob(job)
	return cloneJob(job), nil
}

func (s *jobStore) get(id string) (storedJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return storedJob{}, errJobNotFound
	}
	return cloneJob(job), nil
}

func (s *jobStore) list() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, cloneJob(job).Job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs
}

func (s *jobStore) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.Job.Status == "queued" || job.Job.Status == "running" {
			return true
		}
	}
	return false
}

func (s *jobStore) update(id string, change func(*storedJob)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return errJobNotFound
	}
	job = cloneJob(job)
	change(&job)
	if err := s.persist(job); err != nil {
		return err
	}
	s.jobs[id] = job
	return nil
}

func (s *jobStore) claim() (*storedJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *storedJob
	for _, job := range s.jobs {
		if job.Job.Status == "queued" && (next == nil || job.Job.CreatedAt.Before(next.Job.CreatedAt)) {
			copy := cloneJob(job)
			next = &copy
		}
	}
	if next == nil {
		return nil, nil
	}
	next.Job.Status, next.Job.Phase, next.Job.StartedAt = "running", "building", new(time.Now().UTC())
	if err := s.persist(*next); err != nil {
		return nil, err
	}
	s.jobs[next.Job.ID] = cloneJob(*next)
	return next, nil
}

func (s *jobStore) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return errJobNotFound
	}
	if job.Job.Status == "running" || job.Job.Status == "queued" || job.Job.DeletedAt == nil {
		return errors.New("finish the job and delete its instance before removing the record")
	}
	if err := os.Remove(filepath.Join(s.directory, id+".job")); err != nil {
		return err
	}
	if err := syncDirectory(s.directory); err != nil {
		return err
	}
	delete(s.jobs, id)
	return nil
}
