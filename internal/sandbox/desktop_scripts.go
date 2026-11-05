package sandbox

import "embed"

// desktopScripts holds the shell scripts and XFCE configuration files
// installed inside the sandbox container to bring up a graphical desktop.
//
// Files are embedded at build time so the Docker/Cube backends can upload
// them into the sandbox before starting the VNC server. The structure
// mirrors what the container expects under /tmp/desktop-setup/.
//
//go:embed desktopscripts/*
var desktopScripts embed.FS

// DesktopScriptPaths lists the relative paths inside desktopscripts/ that
// should be uploaded into the sandbox. The start-desktop.sh entry point is
// always included; XFCE config is optional but recommended for a usable
// desktop.
var DesktopScriptPaths = []string{
	"start-desktop.sh",
	"xfce4-desktop.xml",
}
