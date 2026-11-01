package server

import (
	"math"
	"sync"
	"time"

	"github.com/0x7d8/wings/environment"
)

const (
	// cpuBurstEpsilon is the usage margin above baseline before drain starts.
	cpuBurstEpsilon = 0.5
	// cpuBurstHysteresis is the minimum credit required after a drain before
	// the ceiling quota is restored, avoiding rapid ContainerUpdate flaps.
	cpuBurstHysteresis = 5.0
	// cpuBurstUpdateThrottle limits how often we call Docker ContainerUpdate.
	cpuBurstUpdateThrottle = 1500 * time.Millisecond
)

// cpuBurstController tracks per-server burst credit and applies live Docker
// CPUQuota clamps via Environment.InSituUpdate when credit crosses thresholds.
type cpuBurstController struct {
	mu         sync.Mutex
	credit     float64
	atBaseline bool
	lastSample time.Time
	lastUpdate time.Time
}

func newCpuBurstController() *cpuBurstController {
	return &cpuBurstController{credit: 100}
}

// Reset restores a full bucket (e.g. after config sync or process stop).
func (c *cpuBurstController) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credit = 100
	c.atBaseline = false
	c.lastSample = time.Time{}
	c.lastUpdate = time.Time{}
}

// Remaining returns the current credit percent, or nil when burst is disabled.
func (c *cpuBurstController) Remaining(enabled bool) *float64 {
	if !enabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := math.Round(c.credit*10) / 10
	return &v
}

// ApplyQuota writes the current clamp into the environment Limits so the next
// InSituUpdate / Create uses the correct Docker CPUQuota.
func (c *cpuBurstController) ApplyQuota(env environment.ProcessEnvironment, limits environment.Limits) {
	if env == nil || !limits.BurstEnabled() {
		return
	}
	c.mu.Lock()
	atBaseline := c.atBaseline
	c.mu.Unlock()

	quota := limits.CpuLimit
	if atBaseline {
		quota = limits.CpuBaseline
	}
	env.Config().SetAppliedCpuQuota(quota)
}

// Sample updates the token bucket from a resource poll reading and may clamp
// Docker CPUQuota when credit crosses empty or recovers past hysteresis.
func (s *Server) SampleCpuBurst(cpuAbsolute float64) {
	if s.cpuBurst == nil || s.Environment == nil {
		return
	}

	limits := s.Environment.Config().Limits()
	if !limits.BurstEnabled() {
		s.resources.SetCpuBurstRemaining(nil)
		return
	}

	now := time.Now()
	c := s.cpuBurst
	c.mu.Lock()

	dt := 0.0
	if !c.lastSample.IsZero() {
		dt = now.Sub(c.lastSample).Seconds()
	}
	c.lastSample = now

	if dt > 0 && dt < 30 {
		baseline := float64(limits.CpuBaseline)
		burst := float64(limits.CpuBurst)
		burstSeconds := float64(limits.BurstSeconds())
		rechargeSeconds := float64(limits.BurstRechargeSeconds())

		if cpuAbsolute > baseline+cpuBurstEpsilon && burst > 0 && burstSeconds > 0 {
			overage := math.Min(cpuAbsolute-baseline, burst)
			drain := (overage / burst) * (dt / burstSeconds) * 100
			c.credit = math.Max(0, c.credit-drain)
		} else if rechargeSeconds > 0 {
			c.credit = math.Min(100, c.credit+(dt/rechargeSeconds)*100)
		}
	}

	wantBaseline := c.atBaseline
	if c.credit <= 0 {
		c.credit = 0
		wantBaseline = true
	} else if c.atBaseline && c.credit >= cpuBurstHysteresis {
		wantBaseline = false
	} else if !c.atBaseline && c.credit > 0 {
		wantBaseline = false
	}

	changed := wantBaseline != c.atBaseline
	c.atBaseline = wantBaseline
	throttleOK := c.lastUpdate.IsZero() || now.Sub(c.lastUpdate) >= cpuBurstUpdateThrottle
	shouldUpdate := changed && throttleOK
	if shouldUpdate {
		c.lastUpdate = now
	}

	credit := c.credit
	atBaseline := c.atBaseline
	c.mu.Unlock()

	remaining := math.Round(credit*10) / 10
	s.resources.SetCpuBurstRemaining(&remaining)

	quota := limits.CpuLimit
	if atBaseline {
		quota = limits.CpuBaseline
	}
	s.Environment.Config().SetAppliedCpuQuota(quota)

	if !shouldUpdate {
		return
	}

	s.Log().WithField("quota", quota).WithField("credit", remaining).Debug("cpu burst: applying live CPU quota clamp")
	if err := s.Environment.InSituUpdate(); err != nil {
		s.Log().WithField("error", err).Warn("cpu burst: failed to apply live CPU quota clamp")
	}
}
