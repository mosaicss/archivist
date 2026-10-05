//go:build windows

package connect

import "strings"

// envKey is the form environment keys are compared in: Windows keys are
// case-insensitive, so anthropic_api_key is denied like ANTHROPIC_API_KEY
// and Path passes like PATH (Story 78.34).
func envKey(k string) string { return strings.ToUpper(k) }

// platformAllowKeys are the Windows core variables a harness and the
// commands it runs need (shell, system paths, profile, temp, PowerShell
// modules). They hold paths and machine facts, never credentials.
var platformAllowKeys = []string{
	"Path", "PATHEXT", "ComSpec", "windir", "SystemDrive", "SystemRoot", "USERPROFILE",
	"HOMEDRIVE", "HOMEPATH", "USERNAME", "USERDOMAIN", "APPDATA", "LOCALAPPDATA", "ProgramData",
	"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "TEMP", "TMP", "PSModulePath",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
}
