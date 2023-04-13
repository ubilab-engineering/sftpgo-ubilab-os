// Copyright (C) 2019-2023 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package sftpd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/drakkan/sftpgo/v2/internal/common"
	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/metric"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	defaultPrivateRSAKeyName     = "id_rsa"
	defaultPrivateECDSAKeyName   = "id_ecdsa"
	defaultPrivateEd25519KeyName = "id_ed25519"
	sourceAddressCriticalOption  = "source-address"
	kexDHGroupExchangeSHA1       = "diffie-hellman-group-exchange-sha1"
	kexDHGroupExchangeSHA256     = "diffie-hellman-group-exchange-sha256"
)

var (
	sftpExtensions        = []string{"statvfs@openssh.com"}
	supportedHostKeyAlgos = []string{
		ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01,
		ssh.CertAlgoRSAv01, ssh.CertAlgoDSAv01, ssh.CertAlgoECDSA256v01,
		ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01, ssh.CertAlgoED25519v01,
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
		ssh.KeyAlgoRSA, ssh.KeyAlgoDSA,
		ssh.KeyAlgoED25519,
	}
	preferredHostKeyAlgos = []string{
		ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01,
		ssh.CertAlgoECDSA256v01,
		ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01, ssh.CertAlgoED25519v01,
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
		ssh.KeyAlgoED25519,
	}
	supportedKexAlgos = []string{
		"curve25519-sha256", "curve25519-sha256@libssh.org",
		"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
		"diffie-hellman-group14-sha256", "diffie-hellman-group16-sha512",
		"diffie-hellman-group18-sha512", "diffie-hellman-group14-sha1",
		"diffie-hellman-group1-sha1",
	}
	preferredKexAlgos = []string{
		"curve25519-sha256", "curve25519-sha256@libssh.org",
		"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
		"diffie-hellman-group14-sha256",
	}
	supportedCiphers = []string{
		"aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
		"chacha20-poly1305@openssh.com",
		"aes128-ctr", "aes192-ctr", "aes256-ctr",
		"aes128-cbc", "aes192-cbc", "aes256-cbc",
		"3des-cbc",
		"arcfour", "arcfour128", "arcfour256",
	}
	preferredCiphers = []string{
		"aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
		"chacha20-poly1305@openssh.com",
		"aes128-ctr", "aes192-ctr", "aes256-ctr",
	}
	supportedMACs = []string{
		"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256",
		"hmac-sha2-512-etm@openssh.com", "hmac-sha2-512",
		"hmac-sha1", "hmac-sha1-96",
	}
	preferredMACs = []string{
		"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256",
	}

	revokedCertManager = revokedCertificates{
		certs: map[string]bool{},
	}

	sftpAuthError = newAuthenticationError(nil, "")
)

// Binding defines the configuration for a network listener
type Binding struct {
	// The address to listen on. A blank value means listen on all available network interfaces.
	Address string `json:"address" mapstructure:"address"`
	// The port used for serving requests
	Port int `json:"port" mapstructure:"port"`
	// Apply the proxy configuration, if any, for this binding
	ApplyProxyConfig bool `json:"apply_proxy_config" mapstructure:"apply_proxy_config"`
}

// GetAddress returns the binding address
func (b *Binding) GetAddress() string {
	return fmt.Sprintf("%s:%d", b.Address, b.Port)
}

// IsValid returns true if the binding port is > 0
func (b *Binding) IsValid() bool {
	return b.Port > 0
}

// HasProxy returns true if the proxy protocol is active for this binding
func (b *Binding) HasProxy() bool {
	return b.ApplyProxyConfig && common.Config.ProxyProtocol > 0
}

// Configuration for the SFTP server
type Configuration struct {
	// Identification string used by the server
	Banner string `json:"banner" mapstructure:"banner"`
	// Addresses and ports to bind to
	Bindings []Binding `json:"bindings" mapstructure:"bindings"`
	// Maximum number of authentication attempts permitted per connection.
	// If set to a negative number, the number of attempts is unlimited.
	// If set to zero, the number of attempts are limited to 6.
	MaxAuthTries int `json:"max_auth_tries" mapstructure:"max_auth_tries"`
	// HostKeys define the daemon's private host keys.
	// Each host key can be defined as a path relative to the configuration directory or an absolute one.
	// If empty or missing, the daemon will search or try to generate "id_rsa" and "id_ecdsa" host keys
	// inside the configuration directory.
	HostKeys []string `json:"host_keys" mapstructure:"host_keys"`
	// HostCertificates defines public host certificates.
	// Each certificate can be defined as a path relative to the configuration directory or an absolute one.
	// Certificate's public key must match a private host key otherwise it will be silently ignored.
	HostCertificates []string `json:"host_certificates" mapstructure:"host_certificates"`
	// HostKeyAlgorithms lists the public key algorithms that the server will accept for host
	// key authentication.
	HostKeyAlgorithms []string `json:"host_key_algorithms" mapstructure:"host_key_algorithms"`
	// Diffie-Hellman moduli files.
	// Each moduli file can be defined as a path relative to the configuration directory or an absolute one.
	// If set and valid, "diffie-hellman-group-exchange-sha256" and "diffie-hellman-group-exchange-sha1" KEX algorithms
	// will be available, `diffie-hellman-group-exchange-sha256` will be enabled by default if you
	// don't explicitly set KEXs
	Moduli []string `json:"moduli" mapstructure:"moduli"`
	// KexAlgorithms specifies the available KEX (Key Exchange) algorithms in
	// preference order.
	KexAlgorithms []string `json:"kex_algorithms" mapstructure:"kex_algorithms"`
	// Ciphers specifies the ciphers allowed
	Ciphers []string `json:"ciphers" mapstructure:"ciphers"`
	// MACs Specifies the available MAC (message authentication code) algorithms
	// in preference order
	MACs []string `json:"macs" mapstructure:"macs"`
	// TrustedUserCAKeys specifies a list of public keys paths of certificate authorities
	// that are trusted to sign user certificates for authentication.
	// The paths can be absolute or relative to the configuration directory
	TrustedUserCAKeys []string `json:"trusted_user_ca_keys" mapstructure:"trusted_user_ca_keys"`
	// Path to a file containing the revoked user certificates.
	// This file must contain a JSON list with the public key fingerprints of the revoked certificates.
	// Example content:
	// ["SHA256:bsBRHC/xgiqBJdSuvSTNpJNLTISP/G356jNMCRYC5Es","SHA256:119+8cL/HH+NLMawRsJx6CzPF1I3xC+jpM60bQHXGE8"]
	RevokedUserCertsFile string `json:"revoked_user_certs_file" mapstructure:"revoked_user_certs_file"`
	// LoginBannerFile the contents of the specified file, if any, are sent to
	// the remote user before authentication is allowed.
	LoginBannerFile string `json:"login_banner_file" mapstructure:"login_banner_file"`
	// List of enabled SSH commands.
	// We support the following SSH commands:
	// - "scp". SCP is an experimental feature, we have our own SCP implementation since
	//      we can't rely on scp system command to proper handle permissions, quota and
	//      user's home dir restrictions.
	// 		The SCP protocol is quite simple but there is no official docs about it,
	// 		so we need more testing and feedbacks before enabling it by default.
	// 		We may not handle some borderline cases or have sneaky bugs.
	// 		Please do accurate tests yourself before enabling SCP and let us known
	// 		if something does not work as expected for your use cases.
	//      SCP between two remote hosts is supported using the `-3` scp option.
	// - "md5sum", "sha1sum", "sha256sum", "sha384sum", "sha512sum". Useful to check message
	//      digests for uploaded files. These commands are implemented inside SFTPGo so they
	//      work even if the matching system commands are not available, for example on Windows.
	// - "cd", "pwd". Some mobile SFTP clients does not support the SFTP SSH_FXP_REALPATH and so
	//      they use "cd" and "pwd" SSH commands to get the initial directory.
	//      Currently `cd` do nothing and `pwd` always returns the "/" path.
	//
	// The following SSH commands are enabled by default: "md5sum", "sha1sum", "cd", "pwd".
	// "*" enables all supported SSH commands.
	EnabledSSHCommands []string `json:"enabled_ssh_commands" mapstructure:"enabled_ssh_commands"`
	// KeyboardInteractiveAuthentication specifies whether keyboard interactive authentication is allowed.
	// If no keyboard interactive hook or auth plugin is defined the default is to prompt for the user password and then the
	// one time authentication code, if defined.
	KeyboardInteractiveAuthentication bool `json:"keyboard_interactive_authentication" mapstructure:"keyboard_interactive_authentication"`
	// Absolute path to an external program or an HTTP URL to invoke for keyboard interactive authentication.
	// Leave empty to disable this authentication mode.
	KeyboardInteractiveHook string `json:"keyboard_interactive_auth_hook" mapstructure:"keyboard_interactive_auth_hook"`
	// PasswordAuthentication specifies whether password authentication is allowed.
	PasswordAuthentication bool `json:"password_authentication" mapstructure:"password_authentication"`
	// Virtual root folder prefix to include in all file operations (ex: /files).
	// The virtual paths used for per-directory permissions, file patterns etc. must not include the folder prefix.
	// The prefix is only applied to SFTP requests, SCP and other SSH commands will be automatically disabled if
	// you configure a prefix.
	// This setting can help some migrations from OpenSSH. It is not recommended for general usage.
	FolderPrefix     string `json:"folder_prefix" mapstructure:"folder_prefix"`
	certChecker      *ssh.CertChecker
	parsedUserCAKeys []ssh.PublicKey
}

type authenticationError struct {
	err         error
	loginMethod string
}

func (e *authenticationError) Error() string {
	return fmt.Sprintf("Authentication error: %v", e.err)
}

// Is reports if target matches
func (e *authenticationError) Is(target error) bool {
	_, ok := target.(*authenticationError)
	return ok
}

// Unwrap returns the wrapped error
func (e *authenticationError) Unwrap() error {
	return e.err
}

func (e *authenticationError) getLoginMethod() string {
	return e.loginMethod
}

func newAuthenticationError(err error, loginMethod string) *authenticationError {
	return &authenticationError{err: err, loginMethod: loginMethod}
}

// ShouldBind returns true if there is at least a valid binding
func (c *Configuration) ShouldBind() bool {
	for _, binding := range c.Bindings {
		if binding.IsValid() {
			return true
		}
	}

	return false
}

func (c *Configuration) getServerConfig() *ssh.ServerConfig {
	serverConfig := &ssh.ServerConfig{
		NoClientAuth: false,
		MaxAuthTries: c.MaxAuthTries,
		PublicKeyCallback: func(conn ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
			sp, err := c.validatePublicKeyCredentials(conn, pubKey)
			if err == ssh.ErrPartialSuccess {
				return sp, err
			}
			if err != nil {
				return nil, newAuthenticationError(fmt.Errorf("could not validate public key credentials: %w", err),
					dataprovider.SSHLoginMethodPublicKey)
			}

			return sp, nil
		},
		NextAuthMethodsCallback: func(conn ssh.ConnMetadata) []string {
			var nextMethods []string
			user, err := dataprovider.GetUserWithGroupSettings(conn.User(), "")
			if err == nil {
				nextMethods = user.GetNextAuthMethods(conn.PartialSuccessMethods(), c.PasswordAuthentication)
			}
			return nextMethods
		},
		ServerVersion: fmt.Sprintf("SSH-2.0-%s", c.Banner),
	}

	if c.PasswordAuthentication {
		serverConfig.PasswordCallback = func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			sp, err := c.validatePasswordCredentials(conn, pass)
			if err != nil {
				return nil, newAuthenticationError(fmt.Errorf("could not validate password credentials: %w", err),
					dataprovider.SSHLoginMethodPassword)
			}

			return sp, nil
		}
		serviceStatus.Authentications = append(serviceStatus.Authentications, dataprovider.LoginMethodPassword)
	}
	serviceStatus.Authentications = append(serviceStatus.Authentications, dataprovider.SSHLoginMethodPublicKey)

	return serverConfig
}

func (c *Configuration) updateSupportedAuthentications() {
	serviceStatus.Authentications = util.RemoveDuplicates(serviceStatus.Authentications, false)

	if util.Contains(serviceStatus.Authentications, dataprovider.LoginMethodPassword) &&
		util.Contains(serviceStatus.Authentications, dataprovider.SSHLoginMethodPublicKey) {
		serviceStatus.Authentications = append(serviceStatus.Authentications, dataprovider.SSHLoginMethodKeyAndPassword)
	}

	if util.Contains(serviceStatus.Authentications, dataprovider.SSHLoginMethodKeyboardInteractive) &&
		util.Contains(serviceStatus.Authentications, dataprovider.SSHLoginMethodPublicKey) {
		serviceStatus.Authentications = append(serviceStatus.Authentications, dataprovider.SSHLoginMethodKeyAndKeyboardInt)
	}
}

func (c *Configuration) loadFromProvider() error {
	configs, err := dataprovider.GetConfigs()
	if err != nil {
		return fmt.Errorf("unable to load config from provider: %w", err)
	}
	configs.SetNilsToEmpty()
	if len(configs.SFTPD.HostKeyAlgos) > 0 {
		if len(c.HostKeyAlgorithms) == 0 {
			c.HostKeyAlgorithms = preferredHostKeyAlgos
		}
		c.HostKeyAlgorithms = append(c.HostKeyAlgorithms, configs.SFTPD.HostKeyAlgos...)
	}
	c.Moduli = append(c.Moduli, configs.SFTPD.Moduli...)
	if len(configs.SFTPD.KexAlgorithms) > 0 {
		if len(c.KexAlgorithms) == 0 {
			c.KexAlgorithms = preferredKexAlgos
		}
		c.KexAlgorithms = append(c.KexAlgorithms, configs.SFTPD.KexAlgorithms...)
	}
	if len(configs.SFTPD.Ciphers) > 0 {
		if len(c.Ciphers) == 0 {
			c.Ciphers = preferredCiphers
		}
		c.Ciphers = append(c.Ciphers, configs.SFTPD.Ciphers...)
	}
	if len(configs.SFTPD.MACs) > 0 {
		if len(c.MACs) == 0 {
			c.MACs = preferredMACs
		}
		c.MACs = append(c.MACs, configs.SFTPD.MACs...)
	}
	return nil
}

// Initialize the SFTP server and add a persistent listener to handle inbound SFTP connections.
func (c *Configuration) Initialize(configDir string) error {
	if err := c.loadFromProvider(); err != nil {
		return fmt.Errorf("unable to load configs from provider: %w", err)
	}
	serviceStatus = ServiceStatus{}
	serverConfig := c.getServerConfig()

	if !c.ShouldBind() {
		return common.ErrNoBinding
	}

	if err := c.checkAndLoadHostKeys(configDir, serverConfig); err != nil {
		serviceStatus.HostKeys = nil
		return err
	}

	if err := c.initializeCertChecker(configDir); err != nil {
		return err
	}

	c.loadModuli(configDir)

	sftp.SetSFTPExtensions(sftpExtensions...) //nolint:errcheck // we configure valid SFTP Extensions so we cannot get an error

	if err := c.configureSecurityOptions(serverConfig); err != nil {
		return err
	}
	c.configureKeyboardInteractiveAuth(serverConfig)
	c.configureLoginBanner(serverConfig, configDir)
	c.checkSSHCommands()
	c.checkFolderPrefix()

	exitChannel := make(chan error, 1)
	serviceStatus.Bindings = nil

	for _, binding := range c.Bindings {
		if !binding.IsValid() {
			continue
		}
		serviceStatus.Bindings = append(serviceStatus.Bindings, binding)

		go func(binding Binding) {
			addr := binding.GetAddress()
			util.CheckTCP4Port(binding.Port)
			listener, err := net.Listen("tcp", addr)
			if err != nil {
				logger.Warn(logSender, "", "error starting listener on address %v: %v", addr, err)
				exitChannel <- err
				return
			}

			if binding.ApplyProxyConfig && common.Config.ProxyProtocol > 0 {
				proxyListener, err := common.Config.GetProxyListener(listener)
				if err != nil {
					logger.Warn(logSender, "", "error enabling proxy listener: %v", err)
					exitChannel <- err
					return
				}
				listener = proxyListener
			}

			exitChannel <- c.serve(listener, serverConfig)
		}(binding)
	}

	serviceStatus.IsActive = true
	serviceStatus.SSHCommands = c.EnabledSSHCommands
	c.updateSupportedAuthentications()

	return <-exitChannel
}

func (c *Configuration) serve(listener net.Listener, serverConfig *ssh.ServerConfig) error {
	logger.Info(logSender, "", "server listener registered, address: %s", listener.Addr().String())
	var tempDelay time.Duration // how long to sleep on accept failure

	for {
		conn, err := listener.Accept()
		if err != nil {
			// see https://github.com/golang/go/blob/4aa1efed4853ea067d665a952eee77c52faac774/src/net/http/server.go#L3046
			if ne, ok := err.(net.Error); ok && ne.Temporary() { //nolint:staticcheck
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				logger.Warn(logSender, "", "accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			logger.Warn(logSender, "", "unrecoverable accept error: %v", err)
			return err
		}
		tempDelay = 0

		go c.AcceptInboundConnection(conn, serverConfig)
	}
}

func (c *Configuration) configureSecurityOptions(serverConfig *ssh.ServerConfig) error {
	if len(c.HostKeyAlgorithms) == 0 {
		c.HostKeyAlgorithms = preferredHostKeyAlgos
	} else {
		c.HostKeyAlgorithms = util.RemoveDuplicates(c.HostKeyAlgorithms, true)
	}
	for _, hostKeyAlgo := range c.HostKeyAlgorithms {
		if !util.Contains(supportedHostKeyAlgos, hostKeyAlgo) {
			return fmt.Errorf("unsupported host key algorithm %q", hostKeyAlgo)
		}
	}
	serverConfig.HostKeyAlgorithms = c.HostKeyAlgorithms
	serviceStatus.HostKeyAlgos = c.HostKeyAlgorithms

	if len(c.KexAlgorithms) > 0 {
		hasDHGroupKEX := util.Contains(supportedKexAlgos, kexDHGroupExchangeSHA256)
		if !hasDHGroupKEX {
			c.KexAlgorithms = util.Remove(c.KexAlgorithms, kexDHGroupExchangeSHA1)
			c.KexAlgorithms = util.Remove(c.KexAlgorithms, kexDHGroupExchangeSHA256)
		}
		c.KexAlgorithms = util.RemoveDuplicates(c.KexAlgorithms, true)
		for _, kex := range c.KexAlgorithms {
			if !util.Contains(supportedKexAlgos, kex) {
				return fmt.Errorf("unsupported key-exchange algorithm %q", kex)
			}
		}
		serverConfig.KeyExchanges = c.KexAlgorithms
		serviceStatus.KexAlgorithms = c.KexAlgorithms
	} else {
		serviceStatus.KexAlgorithms = preferredKexAlgos
	}
	if len(c.Ciphers) > 0 {
		c.Ciphers = util.RemoveDuplicates(c.Ciphers, true)
		for _, cipher := range c.Ciphers {
			if !util.Contains(supportedCiphers, cipher) {
				return fmt.Errorf("unsupported cipher %q", cipher)
			}
		}
		serverConfig.Ciphers = c.Ciphers
		serviceStatus.Ciphers = c.Ciphers
	} else {
		serviceStatus.Ciphers = preferredCiphers
	}
	if len(c.MACs) > 0 {
		c.MACs = util.RemoveDuplicates(c.MACs, true)
		for _, mac := range c.MACs {
			if !util.Contains(supportedMACs, mac) {
				return fmt.Errorf("unsupported MAC algorithm %q", mac)
			}
		}
		serverConfig.MACs = c.MACs
		serviceStatus.MACs = c.MACs
	} else {
		serviceStatus.MACs = preferredMACs
	}
	return nil
}

func (c *Configuration) configureLoginBanner(serverConfig *ssh.ServerConfig, configDir string) {
	if len(c.LoginBannerFile) > 0 {
		bannerFilePath := c.LoginBannerFile
		if !filepath.IsAbs(bannerFilePath) {
			bannerFilePath = filepath.Join(configDir, bannerFilePath)
		}
		bannerContent, err := os.ReadFile(bannerFilePath)
		if err == nil {
			banner := string(bannerContent)
			serverConfig.BannerCallback = func(conn ssh.ConnMetadata) string {
				return banner
			}
		} else {
			logger.WarnToConsole("unable to read SFTPD login banner file: %v", err)
			logger.Warn(logSender, "", "unable to read login banner file: %v", err)
		}
	}
}

func (c *Configuration) configureKeyboardInteractiveAuth(serverConfig *ssh.ServerConfig) {
	if !c.KeyboardInteractiveAuthentication {
		return
	}
	if c.KeyboardInteractiveHook != "" {
		if !strings.HasPrefix(c.KeyboardInteractiveHook, "http") {
			if !filepath.IsAbs(c.KeyboardInteractiveHook) {
				logger.WarnToConsole("invalid keyboard interactive authentication program: %q must be an absolute path",
					c.KeyboardInteractiveHook)
				logger.Warn(logSender, "", "invalid keyboard interactive authentication program: %q must be an absolute path",
					c.KeyboardInteractiveHook)
				return
			}
			_, err := os.Stat(c.KeyboardInteractiveHook)
			if err != nil {
				logger.WarnToConsole("invalid keyboard interactive authentication program:: %v", err)
				logger.Warn(logSender, "", "invalid keyboard interactive authentication program:: %v", err)
				return
			}
		}
	}
	serverConfig.KeyboardInteractiveCallback = func(conn ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		sp, err := c.validateKeyboardInteractiveCredentials(conn, client)
		if err != nil {
			return nil, newAuthenticationError(fmt.Errorf("could not validate keyboard interactive credentials: %w", err),
				dataprovider.SSHLoginMethodKeyboardInteractive)
		}

		return sp, nil
	}

	serviceStatus.Authentications = append(serviceStatus.Authentications, dataprovider.SSHLoginMethodKeyboardInteractive)
}

func canAcceptConnection(ip string) bool {
	if common.IsBanned(ip, common.ProtocolSSH) {
		logger.Log(logger.LevelDebug, common.ProtocolSSH, "", "connection refused, ip %q is banned", ip)
		return false
	}
	if err := common.Connections.IsNewConnectionAllowed(ip, common.ProtocolSSH); err != nil {
		logger.Log(logger.LevelDebug, common.ProtocolSSH, "", "connection not allowed from ip %q: %v", ip, err)
		return false
	}
	_, err := common.LimitRate(common.ProtocolSSH, ip)
	if err != nil {
		return false
	}
	if err := common.Config.ExecutePostConnectHook(ip, common.ProtocolSSH); err != nil {
		return false
	}
	return true
}

// AcceptInboundConnection handles an inbound connection to the server instance and determines if the request should be served or not.
func (c *Configuration) AcceptInboundConnection(conn net.Conn, config *ssh.ServerConfig) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(logSender, "", "panic in AcceptInboundConnection: %q stack trace: %v", r, string(debug.Stack()))
		}
	}()

	ipAddr := util.GetIPFromRemoteAddress(conn.RemoteAddr().String())
	common.Connections.AddClientConnection(ipAddr)
	defer common.Connections.RemoveClientConnection(ipAddr)

	if !canAcceptConnection(ipAddr) {
		conn.Close()
		return
	}
	// Before beginning a handshake must be performed on the incoming net.Conn
	// we'll set a Deadline for handshake to complete, the default is 2 minutes as OpenSSH
	conn.SetDeadline(time.Now().Add(handshakeTimeout)) //nolint:errcheck

	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		logger.Debug(logSender, "", "failed to accept an incoming connection from ip %q: %v", ipAddr, err)
		checkAuthError(ipAddr, err)
		return
	}
	// handshake completed so remove the deadline, we'll use IdleTimeout configuration from now on
	conn.SetDeadline(time.Time{}) //nolint:errcheck

	defer conn.Close()

	var user dataprovider.User

	// Unmarshal cannot fails here and even if it fails we'll have a user with no permissions
	json.Unmarshal([]byte(sconn.Permissions.Extensions["sftpgo_user"]), &user) //nolint:errcheck

	loginType := sconn.Permissions.Extensions["sftpgo_login_method"]
	connectionID := hex.EncodeToString(sconn.SessionID())

	defer user.CloseFs() //nolint:errcheck
	if err = user.CheckFsRoot(connectionID); err != nil {
		logger.Warn(logSender, connectionID, "unable to check fs root for user %q: %v", user.Username, err)
		return
	}

	logger.Log(logger.LevelInfo, common.ProtocolSSH, connectionID,
		"User %q logged in with %q, from ip %q, client version %q", user.Username, loginType,
		ipAddr, string(sconn.ClientVersion()))
	dataprovider.UpdateLastLogin(&user)

	sshConnection := common.NewSSHConnection(connectionID, conn)
	common.Connections.AddSSHConnection(sshConnection)

	defer common.Connections.RemoveSSHConnection(connectionID)

	go ssh.DiscardRequests(reqs)

	channelCounter := int64(0)
	for newChannel := range chans {
		// If its not a session channel we just move on because its not something we
		// know how to handle at this point.
		if newChannel.ChannelType() != "session" {
			logger.Log(logger.LevelDebug, common.ProtocolSSH, connectionID, "received an unknown channel type: %v",
				newChannel.ChannelType())
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type") //nolint:errcheck
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			logger.Log(logger.LevelWarn, common.ProtocolSSH, connectionID, "could not accept a channel: %v", err)
			continue
		}

		channelCounter++
		sshConnection.UpdateLastActivity()
		// Channels have a type that is dependent on the protocol. For SFTP this is "subsystem"
		// with a payload that (should) be "sftp". Discard anything else we receive ("pty", "shell", etc)
		go func(in <-chan *ssh.Request, counter int64) {
			for req := range in {
				ok := false
				connID := fmt.Sprintf("%s_%d", connectionID, counter)

				switch req.Type {
				case "subsystem":
					if string(req.Payload[4:]) == "sftp" {
						ok = true
						connection := &Connection{
							BaseConnection: common.NewBaseConnection(connID, common.ProtocolSFTP, conn.LocalAddr().String(),
								conn.RemoteAddr().String(), user),
							ClientVersion: string(sconn.ClientVersion()),
							RemoteAddr:    conn.RemoteAddr(),
							LocalAddr:     conn.LocalAddr(),
							channel:       channel,
							folderPrefix:  c.FolderPrefix,
						}
						go c.handleSftpConnection(channel, connection)
					}
				case "exec":
					// protocol will be set later inside processSSHCommand it could be SSH or SCP
					connection := Connection{
						BaseConnection: common.NewBaseConnection(connID, "sshd_exec", conn.LocalAddr().String(),
							conn.RemoteAddr().String(), user),
						ClientVersion: string(sconn.ClientVersion()),
						RemoteAddr:    conn.RemoteAddr(),
						LocalAddr:     conn.LocalAddr(),
						channel:       channel,
						folderPrefix:  c.FolderPrefix,
					}
					ok = processSSHCommand(req.Payload, &connection, c.EnabledSSHCommands)
				}
				if req.WantReply {
					req.Reply(ok, nil) //nolint:errcheck
				}
			}
		}(requests, channelCounter)
	}
}

func (c *Configuration) handleSftpConnection(channel ssh.Channel, connection *Connection) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(logSender, "", "panic in handleSftpConnection: %q stack trace: %v", r, string(debug.Stack()))
		}
	}()
	if err := common.Connections.Add(connection); err != nil {
		errClose := connection.Disconnect()
		logger.Info(logSender, "", "unable to add connection: %v, close err: %v", err, errClose)
		return
	}
	defer common.Connections.Remove(connection.GetID())

	// Create the server instance for the channel using the handler we created above.
	server := sftp.NewRequestServer(channel, c.createHandlers(connection), sftp.WithRSAllocator(),
		sftp.WithStartDirectory(connection.User.Filters.StartDirectory))

	defer server.Close()
	if err := server.Serve(); errors.Is(err, io.EOF) {
		exitStatus := sshSubsystemExitStatus{Status: uint32(0)}
		_, err = channel.SendRequest("exit-status", false, ssh.Marshal(&exitStatus))
		connection.Log(logger.LevelInfo, "connection closed, sent exit status %+v error: %v", exitStatus, err)
	} else if err != nil {
		connection.Log(logger.LevelError, "connection closed with error: %v", err)
	}
}

func (c *Configuration) createHandlers(connection *Connection) sftp.Handlers {
	if c.FolderPrefix != "" {
		prefixMiddleware := newPrefixMiddleware(c.FolderPrefix, connection)

		return sftp.Handlers{
			FileGet:  prefixMiddleware,
			FilePut:  prefixMiddleware,
			FileCmd:  prefixMiddleware,
			FileList: prefixMiddleware,
		}
	}

	return sftp.Handlers{
		FileGet:  connection,
		FilePut:  connection,
		FileCmd:  connection,
		FileList: connection,
	}
}

func checkAuthError(ip string, err error) {
	if authErrors, ok := err.(*ssh.ServerAuthError); ok {
		// check public key auth errors here
		for _, err := range authErrors.Errors {
			var sftpAuthErr *authenticationError
			if errors.As(err, &sftpAuthErr) {
				if sftpAuthErr.getLoginMethod() == dataprovider.SSHLoginMethodPublicKey {
					event := common.HostEventLoginFailed
					if errors.Is(err, util.ErrNotFound) {
						event = common.HostEventUserNotFound
					}
					common.AddDefenderEvent(ip, common.ProtocolSSH, event)
					return
				}
			}
		}
	} else {
		logger.ConnectionFailedLog("", ip, dataprovider.LoginMethodNoAuthTryed, common.ProtocolSSH, err.Error())
		metric.AddNoAuthTryed()
		common.AddDefenderEvent(ip, common.ProtocolSSH, common.HostEventNoLoginTried)
		dataprovider.ExecutePostLoginHook(&dataprovider.User{}, dataprovider.LoginMethodNoAuthTryed, ip, common.ProtocolSSH, err)
	}
}

func loginUser(user *dataprovider.User, loginMethod, publicKey string, conn ssh.ConnMetadata) (*ssh.Permissions, error) {
	connectionID := ""
	if conn != nil {
		connectionID = hex.EncodeToString(conn.SessionID())
	}
	if !filepath.IsAbs(user.HomeDir) {
		logger.Warn(logSender, connectionID, "user %q has an invalid home dir: %q. Home dir must be an absolute path, login not allowed",
			user.Username, user.HomeDir)
		return nil, fmt.Errorf("cannot login user with invalid home dir: %q", user.HomeDir)
	}
	if util.Contains(user.Filters.DeniedProtocols, common.ProtocolSSH) {
		logger.Info(logSender, connectionID, "cannot login user %q, protocol SSH is not allowed", user.Username)
		return nil, fmt.Errorf("protocol SSH is not allowed for user %q", user.Username)
	}
	if user.MaxSessions > 0 {
		activeSessions := common.Connections.GetActiveSessions(user.Username)
		if activeSessions >= user.MaxSessions {
			logger.Info(logSender, "", "authentication refused for user: %q, too many open sessions: %v/%v", user.Username,
				activeSessions, user.MaxSessions)
			return nil, fmt.Errorf("too many open sessions: %v", activeSessions)
		}
	}
	if !user.IsLoginMethodAllowed(loginMethod, common.ProtocolSSH, conn.PartialSuccessMethods()) {
		logger.Info(logSender, connectionID, "cannot login user %q, login method %q is not allowed",
			user.Username, loginMethod)
		return nil, fmt.Errorf("login method %q is not allowed for user %q", loginMethod, user.Username)
	}
	if user.MustSetSecondFactorForProtocol(common.ProtocolSSH) {
		logger.Info(logSender, connectionID, "cannot login user %q, second factor authentication is not set",
			user.Username)
		return nil, fmt.Errorf("second factor authentication is not set for user %q", user.Username)
	}
	remoteAddr := conn.RemoteAddr().String()
	if !user.IsLoginFromAddrAllowed(remoteAddr) {
		logger.Info(logSender, connectionID, "cannot login user %q, remote address is not allowed: %v",
			user.Username, remoteAddr)
		return nil, fmt.Errorf("login for user %q is not allowed from this address: %v", user.Username, remoteAddr)
	}

	json, err := json.Marshal(user)
	if err != nil {
		logger.Warn(logSender, connectionID, "error serializing user info: %v, authentication rejected", err)
		return nil, err
	}
	if publicKey != "" {
		loginMethod = fmt.Sprintf("%v: %v", loginMethod, publicKey)
	}
	p := &ssh.Permissions{}
	p.Extensions = make(map[string]string)
	p.Extensions["sftpgo_user"] = string(json)
	p.Extensions["sftpgo_login_method"] = loginMethod
	return p, nil
}

func (c *Configuration) checkSSHCommands() {
	if util.Contains(c.EnabledSSHCommands, "*") {
		c.EnabledSSHCommands = GetSupportedSSHCommands()
		return
	}
	sshCommands := []string{}
	for _, command := range c.EnabledSSHCommands {
		command = strings.TrimSpace(command)
		if util.Contains(supportedSSHCommands, command) {
			sshCommands = append(sshCommands, command)
		} else {
			logger.Warn(logSender, "", "unsupported ssh command: %q ignored", command)
			logger.WarnToConsole("unsupported ssh command: %q ignored", command)
		}
	}
	c.EnabledSSHCommands = sshCommands
	logger.Debug(logSender, "", "enabled SSH commands %v", c.EnabledSSHCommands)
}

func (c *Configuration) checkFolderPrefix() {
	if c.FolderPrefix != "" {
		c.FolderPrefix = path.Join("/", c.FolderPrefix)
		if c.FolderPrefix == "/" {
			c.FolderPrefix = ""
		}
	}
	if c.FolderPrefix != "" {
		c.EnabledSSHCommands = nil
		logger.Debug(logSender, "", "folder prefix %q configured, SSH commands are disabled", c.FolderPrefix)
	}
}

func (c *Configuration) generateDefaultHostKeys(configDir string) error {
	var err error
	defaultHostKeys := []string{defaultPrivateRSAKeyName, defaultPrivateECDSAKeyName, defaultPrivateEd25519KeyName}
	for _, k := range defaultHostKeys {
		autoFile := filepath.Join(configDir, k)
		if _, err = os.Stat(autoFile); errors.Is(err, fs.ErrNotExist) {
			logger.Info(logSender, "", "No host keys configured and %q does not exist; try to create a new host key", autoFile)
			logger.InfoToConsole("No host keys configured and %q does not exist; try to create a new host key", autoFile)
			if k == defaultPrivateRSAKeyName {
				err = util.GenerateRSAKeys(autoFile)
			} else if k == defaultPrivateECDSAKeyName {
				err = util.GenerateECDSAKeys(autoFile)
			} else {
				err = util.GenerateEd25519Keys(autoFile)
			}
			if err != nil {
				logger.Warn(logSender, "", "error creating host key %q: %v", autoFile, err)
				logger.WarnToConsole("error creating host key %q: %v", autoFile, err)
				return err
			}
		}
		c.HostKeys = append(c.HostKeys, k)
	}

	return err
}

func (c *Configuration) checkHostKeyAutoGeneration(configDir string) error {
	for _, k := range c.HostKeys {
		k = strings.TrimSpace(k)
		if filepath.IsAbs(k) {
			if _, err := os.Stat(k); errors.Is(err, fs.ErrNotExist) {
				keyName := filepath.Base(k)
				switch keyName {
				case defaultPrivateRSAKeyName:
					logger.Info(logSender, "", "try to create non-existent host key %q", k)
					logger.InfoToConsole("try to create non-existent host key %q", k)
					err = util.GenerateRSAKeys(k)
					if err != nil {
						logger.Warn(logSender, "", "error creating host key %q: %v", k, err)
						logger.WarnToConsole("error creating host key %q: %v", k, err)
						return err
					}
				case defaultPrivateECDSAKeyName:
					logger.Info(logSender, "", "try to create non-existent host key %q", k)
					logger.InfoToConsole("try to create non-existent host key %q", k)
					err = util.GenerateECDSAKeys(k)
					if err != nil {
						logger.Warn(logSender, "", "error creating host key %q: %v", k, err)
						logger.WarnToConsole("error creating host key %q: %v", k, err)
						return err
					}
				case defaultPrivateEd25519KeyName:
					logger.Info(logSender, "", "try to create non-existent host key %q", k)
					logger.InfoToConsole("try to create non-existent host key %q", k)
					err = util.GenerateEd25519Keys(k)
					if err != nil {
						logger.Warn(logSender, "", "error creating host key %q: %v", k, err)
						logger.WarnToConsole("error creating host key %q: %v", k, err)
						return err
					}
				default:
					logger.Warn(logSender, "", "non-existent host key %q will not be created", k)
					logger.WarnToConsole("non-existent host key %q will not be created", k)
				}
			}
		}
	}
	if len(c.HostKeys) == 0 {
		if err := c.generateDefaultHostKeys(configDir); err != nil {
			return err
		}
	}
	return nil
}

func (c *Configuration) loadModuli(configDir string) {
	supportedKexAlgos = util.Remove(supportedKexAlgos, kexDHGroupExchangeSHA1)
	supportedKexAlgos = util.Remove(supportedKexAlgos, kexDHGroupExchangeSHA256)
	preferredKexAlgos = util.Remove(preferredKexAlgos, kexDHGroupExchangeSHA256)
	c.Moduli = util.RemoveDuplicates(c.Moduli, false)
	for _, m := range c.Moduli {
		m = strings.TrimSpace(m)
		if !util.IsFileInputValid(m) {
			logger.Warn(logSender, "", "unable to load invalid moduli file %q", m)
			logger.WarnToConsole("unable to load invalid host moduli file %q", m)
			continue
		}
		if !filepath.IsAbs(m) {
			m = filepath.Join(configDir, m)
		}
		logger.Info(logSender, "", "loading moduli file %q", m)
		if err := ssh.ParseModuli(m); err != nil {
			logger.Warn(logSender, "", "ignoring moduli file %q, error: %v", m, err)
			continue
		}
		if !util.Contains(supportedKexAlgos, kexDHGroupExchangeSHA1) {
			supportedKexAlgos = append(supportedKexAlgos, kexDHGroupExchangeSHA1)
		}
		if !util.Contains(supportedKexAlgos, kexDHGroupExchangeSHA256) {
			supportedKexAlgos = append(supportedKexAlgos, kexDHGroupExchangeSHA256)
		}
		if !util.Contains(preferredKexAlgos, kexDHGroupExchangeSHA256) {
			preferredKexAlgos = append(preferredKexAlgos, kexDHGroupExchangeSHA256)
		}
	}
}

// If no host keys are defined we try to use or generate the default ones.
func (c *Configuration) checkAndLoadHostKeys(configDir string, serverConfig *ssh.ServerConfig) error {
	if err := c.checkHostKeyAutoGeneration(configDir); err != nil {
		return err
	}
	hostCertificates, err := c.loadHostCertificates(configDir)
	if err != nil {
		return err
	}
	serviceStatus.HostKeys = nil
	for _, hostKey := range c.HostKeys {
		hostKey = strings.TrimSpace(hostKey)
		if !util.IsFileInputValid(hostKey) {
			logger.Warn(logSender, "", "unable to load invalid host key %q", hostKey)
			logger.WarnToConsole("unable to load invalid host key %q", hostKey)
			continue
		}
		if !filepath.IsAbs(hostKey) {
			hostKey = filepath.Join(configDir, hostKey)
		}
		logger.Info(logSender, "", "Loading private host key %q", hostKey)

		privateBytes, err := os.ReadFile(hostKey)
		if err != nil {
			return err
		}

		private, err := ssh.ParsePrivateKey(privateBytes)
		if err != nil {
			return err
		}
		k := HostKey{
			Path:        hostKey,
			Fingerprint: ssh.FingerprintSHA256(private.PublicKey()),
		}
		serviceStatus.HostKeys = append(serviceStatus.HostKeys, k)
		logger.Info(logSender, "", "Host key %q loaded, type %q, fingerprint %q", hostKey,
			private.PublicKey().Type(), k.Fingerprint)

		// Add private key to the server configuration.
		serverConfig.AddHostKey(private)
		for _, cert := range hostCertificates {
			signer, err := ssh.NewCertSigner(cert, private)
			if err == nil {
				serverConfig.AddHostKey(signer)
				logger.Info(logSender, "", "Host certificate loaded for host key %q, fingerprint %q",
					hostKey, ssh.FingerprintSHA256(signer.PublicKey()))
			}
		}
	}
	var fp []string
	for idx := range serviceStatus.HostKeys {
		h := &serviceStatus.HostKeys[idx]
		fp = append(fp, h.Fingerprint)
	}
	vfs.SetSFTPFingerprints(fp)
	return nil
}

func (c *Configuration) loadHostCertificates(configDir string) ([]*ssh.Certificate, error) {
	var certs []*ssh.Certificate
	for _, certPath := range c.HostCertificates {
		certPath = strings.TrimSpace(certPath)
		if !util.IsFileInputValid(certPath) {
			logger.Warn(logSender, "", "unable to load invalid host certificate %q", certPath)
			logger.WarnToConsole("unable to load invalid host certificate %q", certPath)
			continue
		}
		if !filepath.IsAbs(certPath) {
			certPath = filepath.Join(configDir, certPath)
		}
		certBytes, err := os.ReadFile(certPath)
		if err != nil {
			return certs, fmt.Errorf("unable to load host certificate %q: %w", certPath, err)
		}
		parsed, _, _, _, err := ssh.ParseAuthorizedKey(certBytes)
		if err != nil {
			return nil, fmt.Errorf("unable to parse host certificate %q: %w", certPath, err)
		}
		cert, ok := parsed.(*ssh.Certificate)
		if !ok {
			return nil, fmt.Errorf("the file %q is not an SSH certificate", certPath)
		}
		if cert.CertType != ssh.HostCert {
			return nil, fmt.Errorf("the file %q is not an host certificate", certPath)
		}
		certs = append(certs, cert)
	}
	return certs, nil
}

func (c *Configuration) initializeCertChecker(configDir string) error {
	for _, keyPath := range c.TrustedUserCAKeys {
		keyPath = strings.TrimSpace(keyPath)
		if !util.IsFileInputValid(keyPath) {
			logger.Warn(logSender, "", "unable to load invalid trusted user CA key %q", keyPath)
			logger.WarnToConsole("unable to load invalid trusted user CA key %q", keyPath)
			continue
		}
		if !filepath.IsAbs(keyPath) {
			keyPath = filepath.Join(configDir, keyPath)
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			logger.Warn(logSender, "", "error loading trusted user CA key %q: %v", keyPath, err)
			logger.WarnToConsole("error loading trusted user CA key %q: %v", keyPath, err)
			return err
		}
		parsedKey, _, _, _, err := ssh.ParseAuthorizedKey(keyBytes)
		if err != nil {
			logger.Warn(logSender, "", "error parsing trusted user CA key %q: %v", keyPath, err)
			logger.WarnToConsole("error parsing trusted user CA key %q: %v", keyPath, err)
			return err
		}
		c.parsedUserCAKeys = append(c.parsedUserCAKeys, parsedKey)
	}
	c.certChecker = &ssh.CertChecker{
		SupportedCriticalOptions: []string{
			sourceAddressCriticalOption,
		},
		IsUserAuthority: func(k ssh.PublicKey) bool {
			for _, key := range c.parsedUserCAKeys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return true
				}
			}
			return false
		},
	}
	if c.RevokedUserCertsFile != "" {
		if !util.IsFileInputValid(c.RevokedUserCertsFile) {
			return fmt.Errorf("invalid revoked user certificate: %q", c.RevokedUserCertsFile)
		}
		if !filepath.IsAbs(c.RevokedUserCertsFile) {
			c.RevokedUserCertsFile = filepath.Join(configDir, c.RevokedUserCertsFile)
		}
	}
	revokedCertManager.filePath = c.RevokedUserCertsFile
	return revokedCertManager.load()
}

func (c *Configuration) validatePublicKeyCredentials(conn ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
	var err error
	var user dataprovider.User
	var keyID string
	var sshPerm *ssh.Permissions
	var certPerm *ssh.Permissions

	connectionID := hex.EncodeToString(conn.SessionID())
	method := dataprovider.SSHLoginMethodPublicKey
	ipAddr := util.GetIPFromRemoteAddress(conn.RemoteAddr().String())
	cert, ok := pubKey.(*ssh.Certificate)
	var certFingerprint string
	if ok {
		certFingerprint = ssh.FingerprintSHA256(cert.Key)
		if cert.CertType != ssh.UserCert {
			err = fmt.Errorf("ssh: cert has type %d", cert.CertType)
			user.Username = conn.User()
			updateLoginMetrics(&user, ipAddr, method, err)
			return nil, err
		}
		if !c.certChecker.IsUserAuthority(cert.SignatureKey) {
			err = errors.New("ssh: certificate signed by unrecognized authority")
			user.Username = conn.User()
			updateLoginMetrics(&user, ipAddr, method, err)
			return nil, err
		}
		if len(cert.ValidPrincipals) == 0 {
			err = fmt.Errorf("ssh: certificate %s has no valid principals, user: \"%s\"", certFingerprint, conn.User())
			user.Username = conn.User()
			updateLoginMetrics(&user, ipAddr, method, err)
			return nil, err
		}
		if revokedCertManager.isRevoked(certFingerprint) {
			err = fmt.Errorf("ssh: certificate %s is revoked", certFingerprint)
			user.Username = conn.User()
			updateLoginMetrics(&user, ipAddr, method, err)
			return nil, err
		}
		if err := c.certChecker.CheckCert(conn.User(), cert); err != nil {
			user.Username = conn.User()
			updateLoginMetrics(&user, ipAddr, method, err)
			return nil, err
		}
		certPerm = &cert.Permissions
	}
	if user, keyID, err = dataprovider.CheckUserAndPubKey(conn.User(), pubKey.Marshal(), ipAddr, common.ProtocolSSH, ok); err == nil {
		if ok {
			keyID = fmt.Sprintf("%s: ID: %s, serial: %v, CA %s %s", certFingerprint,
				cert.KeyId, cert.Serial, cert.Type(), ssh.FingerprintSHA256(cert.SignatureKey))
		}
		if user.IsPartialAuth(method) {
			logger.Debug(logSender, connectionID, "user %q authenticated with partial success", conn.User())
			return certPerm, ssh.ErrPartialSuccess
		}
		sshPerm, err = loginUser(&user, method, keyID, conn)
		if err == nil && certPerm != nil {
			// if we have a SSH user cert we need to merge certificate permissions with our ones
			// we only set Extensions, so CriticalOptions are always the ones from the certificate
			sshPerm.CriticalOptions = certPerm.CriticalOptions
			if certPerm.Extensions != nil {
				for k, v := range certPerm.Extensions {
					sshPerm.Extensions[k] = v
				}
			}
		}
	}
	user.Username = conn.User()
	updateLoginMetrics(&user, ipAddr, method, err)
	return sshPerm, err
}

func (c *Configuration) validatePasswordCredentials(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	var err error
	var user dataprovider.User
	var sshPerm *ssh.Permissions

	method := dataprovider.LoginMethodPassword
	if len(conn.PartialSuccessMethods()) == 1 {
		method = dataprovider.SSHLoginMethodKeyAndPassword
	}
	ipAddr := util.GetIPFromRemoteAddress(conn.RemoteAddr().String())
	if user, err = dataprovider.CheckUserAndPass(conn.User(), string(pass), ipAddr, common.ProtocolSSH); err == nil {
		sshPerm, err = loginUser(&user, method, "", conn)
	}
	user.Username = conn.User()
	updateLoginMetrics(&user, ipAddr, method, err)
	return sshPerm, err
}

func (c *Configuration) validateKeyboardInteractiveCredentials(conn ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	var err error
	var user dataprovider.User
	var sshPerm *ssh.Permissions

	method := dataprovider.SSHLoginMethodKeyboardInteractive
	if len(conn.PartialSuccessMethods()) == 1 {
		method = dataprovider.SSHLoginMethodKeyAndKeyboardInt
	}
	ipAddr := util.GetIPFromRemoteAddress(conn.RemoteAddr().String())
	if user, err = dataprovider.CheckKeyboardInteractiveAuth(conn.User(), c.KeyboardInteractiveHook, client,
		ipAddr, common.ProtocolSSH); err == nil {
		sshPerm, err = loginUser(&user, method, "", conn)
	}
	user.Username = conn.User()
	updateLoginMetrics(&user, ipAddr, method, err)
	return sshPerm, err
}

func updateLoginMetrics(user *dataprovider.User, ip, method string, err error) {
	metric.AddLoginAttempt(method)
	if err != nil {
		logger.ConnectionFailedLog(user.Username, ip, method, common.ProtocolSSH, err.Error())
		if method != dataprovider.SSHLoginMethodPublicKey {
			// some clients try all available public keys for a user, we
			// record failed login key auth only once for session if the
			// authentication fails in checkAuthError
			event := common.HostEventLoginFailed
			if errors.Is(err, util.ErrNotFound) {
				event = common.HostEventUserNotFound
			}
			common.AddDefenderEvent(ip, common.ProtocolSSH, event)
		}
	}
	metric.AddLoginResult(method, err)
	dataprovider.ExecutePostLoginHook(user, method, ip, common.ProtocolSSH, err)
}

type revokedCertificates struct {
	filePath string
	mu       sync.RWMutex
	certs    map[string]bool
}

func (r *revokedCertificates) load() error {
	if r.filePath == "" {
		return nil
	}
	logger.Debug(logSender, "", "loading revoked user certificate file %q", r.filePath)
	info, err := os.Stat(r.filePath)
	if err != nil {
		return fmt.Errorf("unable to load revoked user certificate file %q: %w", r.filePath, err)
	}
	maxSize := int64(1048576 * 5) // 5MB
	if info.Size() > maxSize {
		return fmt.Errorf("unable to load revoked user certificate file %q size too big: %v/%v bytes",
			r.filePath, info.Size(), maxSize)
	}
	content, err := os.ReadFile(r.filePath)
	if err != nil {
		return fmt.Errorf("unable to read revoked user certificate file %q: %w", r.filePath, err)
	}
	var certs []string
	err = json.Unmarshal(content, &certs)
	if err != nil {
		return fmt.Errorf("unable to parse revoked user certificate file %q: %w", r.filePath, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.certs = map[string]bool{}
	for _, fp := range certs {
		r.certs[fp] = true
	}
	logger.Debug(logSender, "", "revoked user certificate file %q loaded, entries: %v", r.filePath, len(r.certs))
	return nil
}

func (r *revokedCertificates) isRevoked(fp string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.certs[fp]
}

// Reload reloads the list of revoked user certificates
func Reload() error {
	return revokedCertManager.load()
}
