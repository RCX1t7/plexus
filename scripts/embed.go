// Package scripts embeds the helper scripts shipped inside plexus.exe.
package scripts

import _ "embed"

// InstallTask is install-task.ps1 (Task Scheduler registration at logon).
//
//go:embed install-task.ps1
var InstallTask string
