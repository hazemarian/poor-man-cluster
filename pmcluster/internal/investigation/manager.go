package investigation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// Job is an immutable presentation snapshot of an asynchronous investigation.
type Job struct {
	ID, Owner, Status, Error string
	Created                  time.Time
	Input                    Input
	Verdict                  *Verdict
	Usage                    *int64
	Reserved                 int64
	Used                     int64
}
type work struct {
	job    Job
	policy Policy
	ledger Ledger
	ctx    context.Context
	cancel context.CancelFunc
}

// Manager owns one worker, four queued calls, and bounded in-memory results.
type Manager struct {
	mu        sync.Mutex
	jobs      map[string]*work
	queue     chan *work
	provider  Provider
	key       string
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	redactor  observability.Redactor
	lastStart time.Time
}

// NewManager starts the existing edge process's bounded background worker.
func NewManager(provider Provider, key string, redactor observability.Redactor) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{jobs: map[string]*work{}, queue: make(chan *work, 4), provider: provider, key: key, ctx: ctx, cancel: cancel, done: make(chan struct{}), redactor: redactor.WithExact(key)}
	if key != "" {
		go m.run()
	} else {
		close(m.done)
	}
	return m
}

// Configured reports whether a server-side key is available without revealing it.
func (m *Manager) Configured() bool { return m != nil && m.key != "" }

// Start admits budget and queues a job; polling disconnects do not cancel it.
func (m *Manager) Start(ctx context.Context, ledger Ledger, owner string, p Policy, in Input) (Job, error) {
	if !m.Configured() || !p.Enabled || !p.Sharing || len(in.Evidence) == 0 {
		return Job{}, errors.New("investigation is not configured or evidence unavailable")
	}
	if err := p.Validate(); err != nil {
		return Job{}, err
	}
	r := m.redactor
	in.Question = r.Text(in.Question)
	for i := range in.Evidence {
		in.Evidence[i].Message = r.Text(in.Evidence[i].Message)
		in.Evidence[i].Example = r.Text(in.Evidence[i].Example)
		in.Evidence[i].Service = r.Text(in.Evidence[i].Service)
		in.Evidence[i].Severity = r.Text(in.Evidence[i].Severity)
	}
	if len(in.Question) == 0 || len(in.Question) > 2000 {
		return Job{}, errors.New("question exceeds limit")
	}
	encoded, _ := json.Marshal(in)
	for len(in.Evidence) > 0 && int64(len(encoded)+4096) > p.MaxInputTokens {
		last := in.Evidence[len(in.Evidence)-1]
		in.Stats.OmittedItems++
		in.Stats.OmittedRecords += last.Count
		in.Evidence = in.Evidence[:len(in.Evidence)-1]
		in.Truncated = true
		encoded, _ = json.Marshal(in)
	}
	if len(in.Evidence) == 0 {
		return Job{}, errors.New("input allowance too small")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil || len(m.queue) >= cap(m.queue) || time.Since(m.lastStart) < time.Second {
		return Job{}, errors.New("investigation queue busy")
	}
	for id, w := range m.jobs {
		if w.job.Status != "queued" && w.job.Status != "running" && time.Since(w.job.Created) > 30*time.Minute {
			delete(m.jobs, id)
		}
	}
	if len(m.jobs) >= 64 {
		return Job{}, errors.New("investigation result capacity reached")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Job{}, err
	}
	id := hex.EncodeToString(b)
	admit, cancel := context.WithTimeout(ctx, 5*time.Second)
	reservation, err := ledger.Reserve(admit, id)
	cancel()
	if err != nil {
		return Job{}, errors.New("budget admission unavailable")
	}
	wctx, wcancel := context.WithTimeout(m.ctx, 90*time.Second)
	w := &work{job: Job{ID: id, Owner: owner, Status: "queued", Created: time.Now(), Input: in, Reserved: reservation.Reserved}, policy: p, ledger: ledger, ctx: wctx, cancel: wcancel}
	m.jobs[id] = w
	m.queue <- w
	m.lastStart = time.Now()
	return w.job, nil
}

// Get returns only the submitter's result in local-login mode.
func (m *Manager) Get(id, owner string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.jobs[id]
	if !ok || w.job.Owner != owner {
		return Job{}, false
	}
	return w.job, true
}

// Cancel cancels provider work; accounting conservatively retains possible spend.
func (m *Manager) Cancel(id, owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.jobs[id]
	if !ok || w.job.Owner != owner {
		return false
	}
	if w.job.Status == "queued" || w.job.Status == "running" {
		w.cancel()
	}
	return true
}

// Close cancels calls and drains queue accounting before the edge shuts down.
func (m *Manager) Close() { m.mu.Lock(); m.cancel(); m.mu.Unlock(); <-m.done }

func (m *Manager) run() {
	defer close(m.done)
	for {
		select {
		case w := <-m.queue:
			m.execute(w)
		case <-m.ctx.Done():
			for {
				select {
				case w := <-m.queue:
					m.execute(w)
				default:
					return
				}
			}
		}
	}
}

func (m *Manager) execute(w *work) {
	defer w.cancel()
	var verdict Verdict
	var usage *int64
	var failure error
	if w.ctx.Err() != nil {
		failure = w.ctx.Err()
	} else {
		current, err := w.ledger.Policy(w.ctx)
		if err != nil || current != w.policy || w.job.Reserved != w.policy.MaxInputTokens+w.policy.MaxOutputTokens {
			failure = errors.New("investigation policy changed")
		} else {
			_, failure = w.ledger.Dispatch(w.ctx, w.job.ID)
			if failure == nil {
				m.mu.Lock()
				w.job.Status = "running"
				m.mu.Unlock()
				verdict, usage, failure = m.provider.Run(w.ctx, w.policy, m.key, w.job.Input)
				if failure == nil {
					failure = verdict.Validate(w.job.Input)
				}
			}
		}
	}
	settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, settleErr := w.ledger.Settle(settleCtx, w.job.ID, usage)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	w.job.Usage = usage
	if usage != nil {
		w.job.Used = *usage
	}
	switch {
	case settleErr != nil:
		w.job.Status = "failed"
		w.job.Error = "accounting_unavailable"
	case failure != nil:
		// Only the adapter's own fixed messages reach this line (provider text
		// is never copied into an error), so it is safe to log.
		log.Printf("investigation %s failed: %v", w.job.ID, failure)
		w.job.Status = "failed"
		w.job.Error = "investigation_failed"
	default:
		v := verdict.Redact(m.redactor)
		w.job.Status = "completed"
		w.job.Verdict = &v
	}
}
