package router

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"

	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/server"
)

const (
	nginxSitesAvailable = "/etc/nginx/sites-available"
	nginxSitesEnabled   = "/etc/nginx/sites-enabled"
	serverCertsRoot     = "/srv/server_certs"
	proxyServerMarker   = "# axis-proxy-server: "
)

type LetsEncryptUser struct {
	Email        string
	Registration *registration.Resource
	key          crypto.PrivateKey
}

func (u *LetsEncryptUser) GetEmail() string {
	return u.Email
}
func (u LetsEncryptUser) GetRegistration() *registration.Resource {
	return u.Registration
}
func (u *LetsEncryptUser) GetPrivateKey() crypto.PrivateKey {
	return u.key
}

func proxySiteName(domain, port string) string {
	return domain + "_" + port + ".conf"
}

func proxyAvailablePath(domain, port string) string {
	return filepath.Join(nginxSitesAvailable, proxySiteName(domain, port))
}

func proxyEnabledPath(domain, port string) string {
	return filepath.Join(nginxSitesEnabled, proxySiteName(domain, port))
}

func proxyCertDir(domain string) string {
	return filepath.Join(serverCertsRoot, domain)
}

// reloadNginx validates the nginx config then reloads the service.
func reloadNginx() error {
	if out, err := exec.Command("nginx", "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("nginx config test failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "reload", "nginx").CombinedOutput(); err != nil {
		// Fall back to nginx signal reload when systemctl is unavailable.
		if out2, err2 := exec.Command("nginx", "-s", "reload").CombinedOutput(); err2 != nil {
			return fmt.Errorf(
				"nginx reload failed: systemctl: %w (%s); nginx -s reload: %v (%s)",
				err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)),
			)
		}
	}
	return nil
}

func writeProxySite(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0644); err != nil {
		return err
	}
	enabled := filepath.Join(nginxSitesEnabled, filepath.Base(path))
	_ = os.Remove(enabled)
	if err := os.Symlink(path, enabled); err != nil {
		return fmt.Errorf("symlink sites-enabled: %w", err)
	}
	return nil
}

func removeProxySite(domain, port string) error {
	var first error
	for _, p := range []string{proxyAvailablePath(domain, port), proxyEnabledPath(domain, port)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}

func httpProxyConfig(serverID, domain, ip, port string) []byte {
	return []byte(fmt.Sprintf(`%s%s
server {
	listen 80;
	server_name %s;

	location / {
		proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
		proxy_set_header Host $http_host;
		proxy_pass http://%s:%s;
	}

	location /.well-known/acme-challenge/ {
		proxy_set_header Host $host;
		proxy_pass http://127.0.0.1:81$request_uri;
	}
}
`, proxyServerMarker, serverID, domain, ip, port))
}

func sslProxyConfig(serverID, domain, ip, port, certPath, keyPath string) []byte {
	return []byte(fmt.Sprintf(`%s%s
server {
	listen 80;
	server_name %s;
	return 301 https://$server_name$request_uri;
}

server {
	listen 443 ssl http2;
	server_name %s;

	ssl_certificate %s;
	ssl_certificate_key %s;
	ssl_session_cache shared:SSL:10m;
	ssl_protocols TLSv1.2 TLSv1.3;
	ssl_ciphers "ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305:DHE-RSA-AES128-GCM-SHA256:DHE-RSA-AES256-GCM-SHA384";
	ssl_prefer_server_ciphers on;

	location / {
		proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
		proxy_set_header Host $http_host;
		proxy_pass http://%s:%s;
	}

	location /.well-known/acme-challenge/ {
		proxy_set_header Host $host;
		proxy_pass http://127.0.0.1:81$request_uri;
	}
}
`, proxyServerMarker, serverID, domain, domain, certPath, keyPath, ip, port))
}

func obtainLetsEncryptCertificate(domain, email string) (certPEM, keyPEM []byte, err error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate account key: %w", err)
	}

	letsEncryptUser := LetsEncryptUser{
		Email: email,
		key:   privateKey,
	}

	config := lego.NewConfig(&letsEncryptUser)
	config.Certificate.KeyType = certcrypto.RSA2048

	client, err := lego.NewClient(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create lego client: %w", err)
	}

	if err := client.Challenge.SetHTTP01Provider(http01.NewProviderServer("", "81")); err != nil {
		return nil, nil, fmt.Errorf("set HTTP-01 provider: %w", err)
	}

	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, nil, fmt.Errorf("register ACME account: %w", err)
	}
	letsEncryptUser.Registration = reg

	cert, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{domain},
		Bundle:  true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("obtain certificate: %w", err)
	}

	return cert.Certificate, cert.PrivateKey, nil
}

func saveCertificateFiles(domain string, certPEM, keyPEM []byte) (certPath, keyPath string, err error) {
	dir := proxyCertDir(domain)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func validateProxyIdentity(domain, port string) error {
	if domain == "" || port == "" {
		return fmt.Errorf("domain and port are required")
	}
	if strings.ContainsAny(domain, "/\\") || strings.Contains(domain, "..") {
		return fmt.Errorf("invalid domain")
	}
	if strings.ContainsAny(port, "/\\") || strings.Contains(port, "..") {
		return fmt.Errorf("invalid port")
	}
	return nil
}

// cleanupServerProxies removes nginx sites tagged for the given server UUID.
// Best-effort: used during server deletion.
func cleanupServerProxies(s *server.Server) {
	entries, err := os.ReadDir(nginxSitesAvailable)
	if err != nil {
		return
	}

	marker := proxyServerMarker + s.ID()
	removed := false
	domains := map[string]struct{}{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		path := filepath.Join(nginxSitesAvailable, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(body), marker) {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".conf")
		// site name is {domain}_{port}; domain may contain dots/underscores — split on last _
		idx := strings.LastIndex(name, "_")
		if idx <= 0 {
			_ = os.Remove(path)
			_ = os.Remove(filepath.Join(nginxSitesEnabled, entry.Name()))
			removed = true
			continue
		}
		domain := name[:idx]
		port := name[idx+1:]
		if err := removeProxySite(domain, port); err != nil {
			s.Log().WithField("error", err).WithField("domain", domain).WithField("port", port).
				Warn("failed to remove proxy site during server cleanup")
		} else {
			removed = true
			domains[domain] = struct{}{}
		}
	}

	for domain := range domains {
		if err := os.RemoveAll(proxyCertDir(domain)); err != nil {
			s.Log().WithField("error", err).WithField("domain", domain).
				Warn("failed to remove proxy certificates during server cleanup")
		}
	}

	if removed {
		if err := reloadNginx(); err != nil {
			s.Log().WithField("error", err).Warn("failed to reload nginx after proxy cleanup")
		}
	}
}

// Create a server proxy
func postServerProxyCreate(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var data struct {
		Domain         string `json:"domain"`
		IP             string `json:"ip"`
		Port           string `json:"port"`
		Ssl            bool   `json:"ssl"`
		UseLetsEncrypt bool   `json:"use_lets_encrypt"`
		ClientEmail    string `json:"client_email"`
		SslCert        string `json:"ssl_cert"`
		SslKey         string `json:"ssl_key"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if err := validateProxyIdentity(data.Domain, data.Port); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if data.IP == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "ip is required"})
		return
	}

	available := proxyAvailablePath(data.Domain, data.Port)

	// Always write the HTTP vhost first so ACME HTTP-01 can succeed when SSL is requested.
	if err := writeProxySite(available, httpProxyConfig(s.ID(), data.Domain, data.IP, data.Port)); err != nil {
		s.Log().WithField("error", err).Error("failed to write nginx proxy config")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to write nginx config"})
		return
	}
	if err := reloadNginx(); err != nil {
		s.Log().WithField("error", err).Error("failed to reload nginx after writing HTTP proxy")
		_ = removeProxySite(data.Domain, data.Port)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to reload nginx"})
		return
	}

	if !data.Ssl {
		c.Status(http.StatusAccepted)
		return
	}

	var certfile, keyfile []byte

	if data.UseLetsEncrypt {
		if data.ClientEmail == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "client_email is required for Let's Encrypt"})
			return
		}
		cert, key, err := obtainLetsEncryptCertificate(data.Domain, data.ClientEmail)
		if err != nil {
			s.Log().WithField("error", err).Error("failed to obtain Let's Encrypt certificate")
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		certfile, keyfile = cert, key
	} else {
		if data.SslCert == "" || data.SslKey == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "ssl_cert and ssl_key are required"})
			return
		}
		certfile = []byte(data.SslCert)
		keyfile = []byte(data.SslKey)
	}

	certPath, keyPath, err := saveCertificateFiles(data.Domain, certfile, keyfile)
	if err != nil {
		s.Log().WithField("error", err).Error("failed to save certificate")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Failed to save certificate"})
		return
	}

	if err := writeProxySite(available, sslProxyConfig(s.ID(), data.Domain, data.IP, data.Port, certPath, keyPath)); err != nil {
		s.Log().WithField("error", err).Error("failed to write nginx SSL proxy config")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to write nginx config"})
		return
	}
	if err := reloadNginx(); err != nil {
		s.Log().WithField("error", err).Error("failed to reload nginx after writing SSL proxy")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to reload nginx"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"ssl": gin.H{"issued": true},
	})
}

// Delete a server proxy
func postServerProxyDelete(c *gin.Context) {
	s := middleware.ExtractServer(c)

	var data struct {
		Domain string `json:"domain"`
		Port   string `json:"port"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if err := validateProxyIdentity(data.Domain, data.Port); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := removeProxySite(data.Domain, data.Port); err != nil {
		s.Log().WithField("error", err).Error("failed to remove nginx proxy config")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove nginx config"})
		return
	}

	if err := os.RemoveAll(proxyCertDir(data.Domain)); err != nil {
		s.Log().WithField("error", err).WithField("domain", data.Domain).
			Warn("failed to remove proxy certificates")
	}

	if err := reloadNginx(); err != nil {
		s.Log().WithField("error", err).Error("failed to reload nginx after proxy delete")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to reload nginx"})
		return
	}

	c.Status(http.StatusAccepted)
}
