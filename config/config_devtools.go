package config

// DevToolsConfiguration controls the locked-down per-server developer sidecar.
type DevToolsConfiguration struct {
	// Enabled is the node-wide master switch. When false, all DevTools APIs fail closed.
	Enabled bool `default:"true" json:"enabled" yaml:"enabled"`

	// Image is the sidecar image used when a server does not override it.
	Image string `default:"ghcr.io/dchu096/atlas-devtools:latest" json:"image" yaml:"image"`

	// Memory is the memory limit for the sidecar in MiB.
	Memory int64 `default:"512" json:"memory" yaml:"memory"`

	// Cpu is the CPU limit in percentage points (100 = 1 core). 0 means unlimited.
	Cpu int64 `default:"100" json:"cpu" yaml:"cpu"`

	// PidsLimit caps processes inside the sidecar.
	PidsLimit int64 `default:"64" json:"pids_limit" yaml:"pids_limit"`

	// IdleTimeout is seconds without an active PTY/SSH session before the sidecar is stopped.
	IdleTimeout int64 `default:"900" json:"idle_timeout" yaml:"idle_timeout"`

	// SessionTTL is the hard lifetime in seconds for an ephemeral SSH credential set (0 = 3600).
	SessionTTL int64 `default:"3600" json:"session_ttl" yaml:"session_ttl"`

	// SSHBind is the address the ephemeral SSH listener binds to.
	// When Ports/SSHPorts/PortPool is non-empty, only those ports are tried; otherwise a
	// kernel-assigned high port is used. Never binds port 22.
	SSHBind string `default:"0.0.0.0" json:"ssh_bind" yaml:"ssh_bind"`

	// Ports is the primary SSH port pool pushed by Atlas (firewall-friendly).
	// Empty means Axis may bind any high port.
	Ports []int `json:"ports" yaml:"ports"`

	// SSHPorts is an alias for Ports (accepted for older panel payloads).
	SSHPorts []int `json:"ssh_ports" yaml:"ssh_ports"`

	// PortPool is an alias for Ports (accepted for older panel payloads).
	PortPool []int `json:"port_pool" yaml:"port_pool"`

	// TmpfsSize is the size of the /tmp tmpfs in MiB.
	TmpfsSize uint `default:"64" json:"tmpfs_size" yaml:"tmpfs_size"`

	// Workdir is the working directory for PTY sessions (server data mount).
	Workdir string `default:"/home/container" json:"workdir" yaml:"workdir"`

	// Shell is the command executed for PTY attach.
	Shell []string `default:"[\"/bin/bash\",\"-l\"]" json:"shell" yaml:"shell"`
}

// SSHPortPool returns the configured ephemeral SSH port pool.
// Uses the first non-empty among Ports, SSHPorts, and PortPool; deduplicates;
// keeps only ports in 1..65535 excluding 22. An empty result means random bind.
func (c DevToolsConfiguration) SSHPortPool() []int {
	for _, pool := range [][]int{c.Ports, c.SSHPorts, c.PortPool} {
		if len(pool) == 0 {
			continue
		}
		return SanitizeSSHPorts(pool)
	}
	return nil
}

// SanitizeSSHPorts returns unique valid DevTools SSH ports from pools in order.
// Invalid ports (outside 1..65535) and port 22 are dropped.
func SanitizeSSHPorts(pools ...[]int) []int {
	seen := make(map[int]struct{})
	out := make([]int, 0)
	for _, pool := range pools {
		for _, p := range pool {
			if !IsValidDevToolsSSHPort(p) {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// IsValidDevToolsSSHPort reports whether p may be used for ephemeral DevTools SSH.
// Port 22 is always rejected.
func IsValidDevToolsSSHPort(p int) bool {
	return p >= 1 && p <= 65535 && p != 22
}

// FirstNonEmptySSHPortPool returns SanitizeSSHPorts of the first non-empty raw pool.
func FirstNonEmptySSHPortPool(pools ...[]int) []int {
	for _, pool := range pools {
		if len(pool) == 0 {
			continue
		}
		return SanitizeSSHPorts(pool)
	}
	return nil
}
