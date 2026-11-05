package browserskill

import "errors"

// Common errors used throughout the browserskill package.
var (
	// ErrBrowserNotConnected is returned when no browser is connected for a user.
	ErrBrowserNotConnected = errors.New("browserskill: no browser connected")

	// ErrBrowserUnavailable is returned when the browser skill is disabled or
	// no manager is configured.
	ErrBrowserUnavailable = errors.New("browserskill: browser skill unavailable")

	// ErrTaskNotFound is returned when the referenced task does not exist.
	ErrTaskNotFound = errors.New("browserskill: task not found")

	// ErrTaskCancelled is returned when a task is cancelled before completion.
	ErrTaskCancelled = errors.New("browserskill: task cancelled")

	// ErrTaskTimeout is returned when a task exceeds its deadline.
	ErrTaskTimeout = errors.New("browserskill: task timeout")

	// ErrPairingTokenInvalid is returned when a pairing token is unknown,
	// expired, or already consumed.
	ErrPairingTokenInvalid = errors.New("browserskill: invalid pairing token")

	// ErrDeviceNotFound is returned when a device ID does not match any known device.
	ErrDeviceNotFound = errors.New("browserskill: device not found")

	// ErrDeviceNotPaired is returned when a connecting device has no valid pairing.
	ErrDeviceNotPaired = errors.New("browserskill: device not paired")

	// ErrHumanHelpRequested is returned (as a sentinel) when a task requests
	// human help and will be resumed later.
	ErrHumanHelpRequested = errors.New("browserskill: human help requested")
)
