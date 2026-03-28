// Package svcmgr installs and manages the torSync OS service.
// Supports Linux (systemd), macOS (launchd), and Windows (sc.exe).
package svcmgr

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

const serviceName = "torsync"

// Install registers torSync as a system service that starts on boot.
// Must be run as root/Administrator.
func Install(binaryPath, configPath string) error {
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("svcmgr: resolve binary path: %w", err)
	}
	switch runtime.GOOS {
	case "linux":
		return installSystemd(abs, configPath)
	case "darwin":
		return installLaunchd(abs, configPath)
	case "windows":
		return installWindows(abs, configPath)
	default:
		return fmt.Errorf("svcmgr: unsupported platform: %s", runtime.GOOS)
	}
}

// Start starts the torSync service via the OS service manager.
func Start() error {
	switch runtime.GOOS {
	case "linux":
		return run("systemctl", "start", serviceName)
	case "darwin":
		return run("launchctl", "start", "com.torsync.agent")
	case "windows":
		return run("sc.exe", "start", serviceName)
	default:
		return errors.New("svcmgr: unsupported platform")
	}
}

// Stop stops the torSync service.
func Stop() error {
	switch runtime.GOOS {
	case "linux":
		return run("systemctl", "stop", serviceName)
	case "darwin":
		return run("launchctl", "stop", "com.torsync.agent")
	case "windows":
		return run("sc.exe", "stop", serviceName)
	default:
		return errors.New("svcmgr: unsupported platform")
	}
}

// Uninstall removes the torSync service registration.
func Uninstall() error {
	switch runtime.GOOS {
	case "linux":
		_ = run("systemctl", "stop", serviceName)
		_ = run("systemctl", "disable", serviceName)
		os.Remove("/etc/systemd/system/torsync.service")
		return run("systemctl", "daemon-reload")
	case "darwin":
		plistPath := "/Library/LaunchDaemons/com.torsync.agent.plist"
		_ = run("launchctl", "unload", plistPath)
		return os.Remove(plistPath)
	case "windows":
		_ = run("sc.exe", "stop", serviceName)
		return run("sc.exe", "delete", serviceName)
	default:
		return errors.New("svcmgr: unsupported platform")
	}
}

// Status prints the current service status to stdout.
func Status() error {
	switch runtime.GOOS {
	case "linux":
		return run("systemctl", "status", serviceName)
	case "darwin":
		return run("launchctl", "print", "system/com.torsync.agent")
	case "windows":
		return run("sc.exe", "query", serviceName)
	default:
		return errors.New("svcmgr: unsupported platform")
	}
}

// ─── Linux systemd ────────────────────────────────────────────────────────────

const systemdUnit = `[Unit]
Description=torSync distributed file synchronisation agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.Binary}} start --config {{.Config}}
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=torsync

[Install]
WantedBy=multi-user.target
`

func installSystemd(binary, configPath string) error {
	unit, err := renderTemplate(systemdUnit, map[string]string{
		"Binary": binary,
		"Config": configPath,
	})
	if err != nil {
		return err
	}

	path := "/etc/systemd/system/torsync.service"
	if err := os.WriteFile(path, []byte(unit), 0644); err != nil {
		return fmt.Errorf("svcmgr: write unit file: %w", err)
	}

	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", serviceName); err != nil {
		return err
	}
	fmt.Printf("systemd service installed. Run: systemctl start %s\n", serviceName)
	return nil
}

// ─── macOS launchd ────────────────────────────────────────────────────────────

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.torsync.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Binary}}</string>
    <string>start</string>
    <string>--config</string>
    <string>{{.Config}}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/var/log/torsync.log</string>
  <key>StandardErrorPath</key>
  <string>/var/log/torsync.log</string>
</dict>
</plist>
`

func installLaunchd(binary, configPath string) error {
	plist, err := renderTemplate(launchdPlist, map[string]string{
		"Binary": binary,
		"Config": configPath,
	})
	if err != nil {
		return err
	}

	path := "/Library/LaunchDaemons/com.torsync.agent.plist"
	if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
		return fmt.Errorf("svcmgr: write plist: %w", err)
	}

	if err := run("launchctl", "load", path); err != nil {
		return err
	}
	fmt.Println("launchd service installed and loaded.")
	return nil
}

// ─── Windows ──────────────────────────────────────────────────────────────────

func installWindows(binary, configPath string) error {
	binPath := fmt.Sprintf(`"%s" start --config "%s"`, binary, configPath)
	if err := run("sc.exe", "create", serviceName,
		"binPath=", binPath,
		"start=", "auto",
		"DisplayName=", "torSync Agent",
	); err != nil {
		return err
	}
	if err := run("sc.exe", "description", serviceName,
		"torSync distributed file synchronisation agent",
	); err != nil {
		return err
	}
	fmt.Printf("Windows service created. Run: sc.exe start %s\n", serviceName)
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("svcmgr: %s %v: %w", name, args, err)
	}
	return nil
}

func renderTemplate(tmpl string, data any) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("svcmgr: parse template: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("svcmgr: render template: %w", err)
	}
	return buf.String(), nil
}
