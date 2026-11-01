package devtools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	errdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/environment"
)

// ContractVersion is the API contract identifier returned to Atlas.
const ContractVersion = "devtools.v1"

// AllowlistedTools documents tools expected in the default sidecar image.
var AllowlistedTools = []string{
	"git", "curl", "wget", "unzip", "tar", "nano", "vim", "less", "jq", "fastfetch",
}

// State values reported to the panel.
const (
	StateDisabled     = "disabled"
	StateStopped      = "stopped"
	StateStarting     = "starting"
	StateRunning      = "running"
	StateError        = "error"
	StateImageMissing = "image_missing"
)

// Status is the panel-facing status payload.
type Status struct {
	Contract         string         `json:"contract"`
	Enabled          bool           `json:"enabled"`
	FeatureAvailable bool           `json:"feature_available"`
	State            string         `json:"state"`
	Container        *ContainerInfo `json:"container"`
	Image            string         `json:"image"`
	Error            *string        `json:"error"`
	IdleTimeoutSecs  int64          `json:"idle_timeout_seconds"`
	Tools            []string       `json:"tools"`
	SSH              *SSHInfo       `json:"ssh"`
}

// ContainerInfo identifies the running sidecar.
type ContainerInfo struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// Manager owns the lifecycle of a single server's DevTools sidecar.
type Manager struct {
	mu sync.Mutex

	serverID      string
	dataPath      string
	log           log.Interface
	client        *client.Client
	enabled       bool
	imageOverride string

	containerID string
	lastError   string
	state       string

	attachCount int
	idleTimer   *time.Timer

	ssh *sshGateway
}

// New creates a DevTools manager for a server.
func New(serverID, dataPath string, l log.Interface) (*Manager, error) {
	cli, err := environment.Docker()
	if err != nil {
		return nil, err
	}
	if l == nil {
		l = log.WithField("subsystem", "devtools").WithField("server", serverID)
	}
	return &Manager{
		serverID: serverID,
		dataPath: dataPath,
		log:      l,
		client:   cli,
		state:    StateDisabled,
	}, nil
}

// dataVolumeName is a local named volume that bind-mounts the server data
// directory with noexec/nosuid/nodev. Docker rejects those flags on bind
// "mode" strings (Binds / TypeBind); the local volume driver accepts them
// via DriverOpts and applies them with mount(2).
func (m *Manager) dataVolumeName() string {
	return m.ContainerName() + "_data"
}

// ContainerName returns the deterministic Docker name for this sidecar.
func (m *Manager) ContainerName() string {
	return m.serverID + "_devtools"
}

// SetEnabled updates the panel-synced enable flag and reconciles state.
func (m *Manager) SetEnabled(ctx context.Context, enabled bool) error {
	m.mu.Lock()
	m.enabled = enabled
	m.mu.Unlock()
	return m.Reconcile(ctx)
}

// SetImageOverride sets an optional per-server image from panel settings.
func (m *Manager) SetImageOverride(image string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageOverride = strings.TrimSpace(image)
}

// Enabled reports whether the server has DevTools enabled.
func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

func (m *Manager) cfg() config.DevToolsConfiguration {
	return config.Get().System.DevTools
}

func (m *Manager) imageName() string {
	if m.imageOverride != "" {
		return m.imageOverride
	}
	return m.cfg().Image
}

// Status returns the current sidecar status.
func (m *Manager) Status(ctx context.Context) Status {
	cfg := m.cfg()
	m.mu.Lock()
	defer m.mu.Unlock()

	st := Status{
		Contract:         ContractVersion,
		Enabled:          m.enabled,
		FeatureAvailable: cfg.Enabled,
		State:            m.state,
		Image:            m.imageName(),
		IdleTimeoutSecs:  cfg.IdleTimeout,
		Tools:            AllowlistedTools,
		Container: &ContainerInfo{
			Name: m.ContainerName(),
			ID:   m.containerID,
		},
	}

	if m.ssh != nil && !m.ssh.closed.Load() {
		st.SSH = &SSHInfo{
			Active:             true,
			Port:               m.ssh.port,
			Username:           m.ssh.username,
			Password:           m.ssh.password,
			ExpiresAt:          m.ssh.expires.UTC().Format(time.RFC3339),
			IdleTimeoutSeconds: m.ssh.idleSecs,
			SessionTTLSeconds:  m.ssh.ttlSecs,
		}
	} else {
		st.SSH = &SSHInfo{Active: false}
	}

	if !cfg.Enabled {
		st.State = StateDisabled
		st.FeatureAvailable = false
	} else if !m.enabled {
		st.State = StateDisabled
	} else if m.lastError != "" && m.state == StateError {
		err := m.lastError
		st.Error = &err
	}

	// Refresh from Docker when we think something exists.
	if m.containerID != "" || m.enabled {
		if id, running, err := m.inspectLocked(ctx); err == nil {
			m.containerID = id
			if running {
				m.state = StateRunning
				st.State = StateRunning
			} else if m.enabled && cfg.Enabled {
				m.state = StateStopped
				st.State = StateStopped
			}
			st.Container.ID = id
		}
	}

	return st
}

func (m *Manager) inspectLocked(ctx context.Context) (id string, running bool, err error) {
	insp, err := m.client.ContainerInspect(ctx, m.ContainerName())
	if err != nil {
		if errdefs.IsNotFound(err) {
			m.containerID = ""
			return "", false, err
		}
		return "", false, err
	}
	return insp.ID, insp.State != nil && insp.State.Running, nil
}

// Reconcile applies enable/disable policy.
func (m *Manager) Reconcile(ctx context.Context) error {
	cfg := m.cfg()
	m.mu.Lock()
	defer m.mu.Unlock()

	if !cfg.Enabled || !m.enabled {
		return m.destroyLocked(ctx)
	}
	// Enabled: leave stopped until attach/start; just clear disabled state.
	if m.state == StateDisabled || m.state == "" {
		m.state = StateStopped
		m.lastError = ""
	}
	return nil
}

// EnsureRunning pulls/creates/starts the sidecar for attach.
func (m *Manager) EnsureRunning(ctx context.Context) error {
	cfg := m.cfg()
	m.mu.Lock()
	defer m.mu.Unlock()

	if !cfg.Enabled {
		return errors.New("devtools: feature disabled on this node")
	}
	if !m.enabled {
		return errors.New("devtools: not enabled for this server")
	}

	m.state = StateStarting
	m.lastError = ""

	if err := m.ensureImageLocked(ctx); err != nil {
		m.state = StateImageMissing
		m.lastError = err.Error()
		return err
	}

	if err := m.createLocked(ctx); err != nil {
		m.state = StateError
		m.lastError = err.Error()
		return err
	}

	if err := m.startLocked(ctx); err != nil {
		m.state = StateError
		m.lastError = err.Error()
		return err
	}

	m.state = StateRunning
	return nil
}

// Start is an explicit power start.
func (m *Manager) Start(ctx context.Context) error {
	return m.EnsureRunning(ctx)
}

// Stop stops the container but keeps it for a fast restart.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(ctx)
}

// Destroy removes the sidecar entirely.
func (m *Manager) Destroy(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.destroyLocked(ctx)
}

func (m *Manager) stopLocked(ctx context.Context) error {
	timeout := 10
	err := m.client.ContainerStop(ctx, m.ContainerName(), container.StopOptions{Timeout: &timeout})
	if err != nil && !errdefs.IsNotFound(err) {
		return errors.Wrap(err, "devtools: failed to stop sidecar")
	}
	if m.enabled && m.cfg().Enabled {
		m.state = StateStopped
	} else {
		m.state = StateDisabled
	}
	return nil
}

func (m *Manager) destroyLocked(ctx context.Context) error {
	m.cancelIdleLocked()
	if m.ssh != nil {
		gw := m.ssh
		m.ssh = nil
		go gw.close()
	}
	_ = m.client.ContainerRemove(ctx, m.ContainerName(), container.RemoveOptions{Force: true})
	_ = m.client.VolumeRemove(ctx, m.dataVolumeName(), true)
	m.containerID = ""
	if m.enabled && m.cfg().Enabled {
		m.state = StateStopped
	} else {
		m.state = StateDisabled
	}
	m.lastError = ""
	return nil
}

// ensureDataVolumeLocked creates the noexec data volume if missing.
func (m *Manager) ensureDataVolumeLocked(ctx context.Context) error {
	name := m.dataVolumeName()
	_, err := m.client.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return errors.Wrap(err, "devtools: inspect data volume")
	}

	_, err = m.client.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		DriverOpts: map[string]string{
			"type":   "none",
			"device": m.dataPath,
			"o":      "bind,rw,noexec,nosuid,nodev",
		},
		Labels: map[string]string{
			"Service":       "Axis",
			"ContainerType": "devtools_sidecar_data",
			"ServerId":      m.serverID,
		},
	})
	if err != nil {
		return errors.Wrap(err, "devtools: create data volume")
	}
	return nil
}

func (m *Manager) ensureImageLocked(ctx context.Context) error {
	img := m.imageName()
	images, err := m.client.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", img)),
	})
	if err == nil && len(images) > 0 {
		return nil
	}

	m.log.WithField("image", img).Info("pulling devtools sidecar image")
	out, err := m.client.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		// Last chance: local image present under an exact RepoTag match.
		all, lerr := m.client.ImageList(ctx, image.ListOptions{})
		if lerr == nil {
			for _, im := range all {
				for _, t := range im.RepoTags {
					if t == img {
						return nil
					}
				}
			}
		}
		return errors.Wrapf(err, "devtools: image %q missing and pull failed", img)
	}
	defer out.Close()
	buf := make([]byte, 4096)
	for {
		_, rerr := out.Read(buf)
		if rerr != nil {
			break
		}
	}
	return nil
}

func (m *Manager) createLocked(ctx context.Context) error {
	if _, _, err := m.inspectLocked(ctx); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}

	cfg := m.cfg()
	sys := config.Get()
	img := m.imageName()

	user := strconv.Itoa(sys.System.User.Uid) + ":" + strconv.Itoa(sys.System.User.Gid)
	if sys.System.User.Rootless.Enabled {
		user = fmt.Sprintf("%d:%d", sys.System.User.Rootless.ContainerUID, sys.System.User.Rootless.ContainerGID)
	}

	pids := cfg.PidsLimit
	resources := container.Resources{
		Memory:    cfg.Memory * 1024 * 1024,
		PidsLimit: &pids,
	}
	if cfg.Cpu > 0 {
		resources.CPUQuota = cfg.Cpu * 1000
		resources.CPUPeriod = 100000
	}

	conf := &container.Config{
		Hostname:     m.ContainerName(),
		Image:        img,
		User:         user,
		WorkingDir:   cfg.Workdir,
		Tty:          true,
		AttachStdin:  false,
		AttachStdout: false,
		AttachStderr: false,
		OpenStdin:    false,
		Env: []string{
			"HOME=" + cfg.Workdir,
			"TERM=xterm-256color",
			"TZ=" + sys.System.Timezone,
		},
		Labels: map[string]string{
			"Service":       "Axis",
			"ContainerType": "devtools_sidecar",
			"ServerId":      m.serverID,
		},
		// Keep container alive until stop; PTY uses docker exec.
		Cmd: []string{"tail", "-f", "/dev/null"},
	}

	// Docker rejects noexec/nosuid/nodev on bind "mode" (Binds / TypeBind).
	// Use a local named volume with DriverOpts so mount(2) gets those flags.
	if err := m.ensureDataVolumeLocked(ctx); err != nil {
		return err
	}

	hostConf := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: m.dataVolumeName(),
				Target: cfg.Workdir,
			},
		},
		Tmpfs: map[string]string{
			"/tmp": fmt.Sprintf("rw,noexec,nosuid,nodev,size=%dM", cfg.TmpfsSize),
		},
		Resources:      resources,
		DNS:            sys.Docker.Network.Dns,
		LogConfig:      sys.Docker.ContainerLogConfig(),
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		NetworkMode:    container.NetworkMode(sys.Docker.Network.Mode),
		UsernsMode:     container.UsernsMode(sys.Docker.UsernsMode),
		Privileged:     false,
	}

	r, err := m.client.ContainerCreate(ctx, conf, hostConf, nil, nil, m.ContainerName())
	if err != nil {
		return errors.Wrap(err, "devtools: failed to create sidecar")
	}
	m.containerID = r.ID
	m.log.WithField("container_id", r.ID).Info("created devtools sidecar")
	return nil
}

func (m *Manager) startLocked(ctx context.Context) error {
	id, running, err := m.inspectLocked(ctx)
	if err != nil {
		return errors.Wrap(err, "devtools: sidecar missing after create")
	}
	m.containerID = id
	if running {
		return nil
	}
	if err := m.client.ContainerStart(ctx, m.ContainerName(), container.StartOptions{}); err != nil {
		return errors.Wrap(err, "devtools: failed to start sidecar")
	}
	return nil
}

// NoteAttach increments the active PTY counter and cancels idle stop.
func (m *Manager) NoteAttach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachCount++
	m.cancelIdleLocked()
}

// NoteDetach decrements the PTY counter and arms idle stop when zero.
func (m *Manager) NoteDetach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attachCount > 0 {
		m.attachCount--
	}
	if m.attachCount == 0 {
		m.armIdleLocked()
	}
}

func (m *Manager) cancelIdleLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
}

func (m *Manager) armIdleLocked() {
	m.cancelIdleLocked()
	timeout := m.cfg().IdleTimeout
	if timeout <= 0 {
		return
	}
	m.idleTimer = time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.attachCount > 0 {
			return
		}
		m.log.Info("devtools idle timeout reached; stopping sidecar")
		if m.ssh != nil {
			gw := m.ssh
			m.ssh = nil
			go gw.close()
		}
		_ = m.stopLocked(ctx)
	})
}

// Shell returns the configured PTY shell command.
func (m *Manager) Shell() []string {
	shell := m.cfg().Shell
	if len(shell) == 0 {
		return []string{"/bin/bash", "-l"}
	}
	return shell
}

// Workdir returns the container working directory.
func (m *Manager) Workdir() string {
	return m.cfg().Workdir
}

// Client exposes the docker client for exec attach.
func (m *Manager) Client() *client.Client {
	return m.client
}
