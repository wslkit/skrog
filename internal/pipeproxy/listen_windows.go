//go:build windows

package pipeproxy

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// DefaultPipeName is what stock docker.exe connects to when DOCKER_HOST is
// unset. Skrog claims it only when Docker Desktop has not; the installer
// falls back to FallbackPipeName so the two coexist (PLAN §02).
const DefaultPipeName = `\\.\pipe\docker_engine`

// FallbackPipeName is Skrog's own pipe, used when the default is taken.
const FallbackPipeName = `\\.\pipe\skrog_engine`

// defaultSDDL builds the pipe's security descriptor: full control for SYSTEM,
// administrators, and the OWNING USER; nobody else connects at all.
//
// Pipe access is equivalent to root inside the engine VM, which in turn can
// read and write anything the automounted drives expose. v0.2.0 granted
// GENERIC_ALL to INTERACTIVE — a SID present in *every* interactively
// logged-on user's token — which crossed the session boundary twice (#79):
// another logged-on account (RDP, fast user switching) could drive this
// user's engine, and because GA includes FILE_CREATE_PIPE_INSTANCE it could
// even create competing server instances of the live pipe and intercept
// docker.exe connections. Scoping to the owner's SID closes both; the grant
// is GA because the owner legitimately both connects (client) and serves
// (this process) — instance creation by the same user is not an escalation.
//
// TODO(#8): once `skrog install` creates the local "Skrog Users" group, a
// GR|GW grant for that group lands here, matching Docker's docker-users
// pattern, so OTHER accounts can be admitted deliberately with client-only
// rights rather than by virtue of being interactive.
func defaultSDDL() (string, error) {
	u, err := currentUserSID()
	if err != nil {
		return "", fmt.Errorf("pipeproxy: resolving the current user for the pipe ACL: %w", err)
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + u + ")", nil
}

// currentUserSID is the SID string of the process token's user.
func currentUserSID() (string, error) {
	t := windows.GetCurrentProcessToken()
	tu, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// Listen creates the named pipe. An empty sddl applies the default (SYSTEM,
// administrators, and the owning user); pass a custom descriptor only with a
// reason, since this is the security boundary.
func Listen(pipeName, sddl string) (net.Listener, error) {
	if pipeName == "" {
		return nil, fmt.Errorf("pipeproxy: empty pipe name")
	}
	if sddl == "" {
		var err error
		if sddl, err = defaultSDDL(); err != nil {
			// Refuse rather than fall back to a wider descriptor: serving the
			// engine to the wrong audience is worse than not serving it.
			return nil, err
		}
	}

	l, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		// Message mode, for one specific reason: it is the only way a named
		// pipe can carry a half-close. The docker CLI signals stdin-EOF on a
		// hijacked interactive stream (docker run -i, exec -i, a pipe into a
		// container) by calling CloseWrite on its connection; on a byte-mode
		// winio pipe that is a silent no-op, so the container's stdin never
		// ends and the command hangs forever (#57). In message mode winio
		// implements CloseWrite as a zero-byte message the reader surfaces as
		// io.EOF — which the relay already propagates to the engine.
		//
		// This does NOT reframe large payloads: winio presents a message-mode
		// pipe as a byte stream for ordinary reads (it swallows
		// ERROR_MORE_DATA), and a zero-length data write sends nothing, so
		// only an explicit CloseWrite ever signals EOF. Image pull/push,
		// build contexts and every hijack path are exercised by the
		// acceptance suite under this mode.
		MessageMode: true,
	})
	if err != nil {
		return nil, fmt.Errorf("pipeproxy: listen %s: %w", pipeName, err)
	}
	return l, nil
}

func init() {
	// Ordinary shutdown shapes on Windows, not faults worth logging:
	// winio.ErrFileClosed is what a winio pipe returns when the relay closes
	// its other direction; ERROR_BROKEN_PIPE ("the pipe has been ended") and
	// ERROR_NO_DATA ("the pipe is being closed") are what a pipe or hvsock
	// read/write returns when the peer went away abruptly — the normal end of
	// a docker CLI that exits mid-connection.
	platformClosedErrors = append(platformClosedErrors,
		winio.ErrFileClosed, windows.ERROR_BROKEN_PIPE, windows.ERROR_NO_DATA)
}
