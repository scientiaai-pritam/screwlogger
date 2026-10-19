package agent

// ForegroundSource reports the executable name of the foreground window.
type ForegroundSource interface {
	ForegroundApp() string
}

// IdleSource reports seconds since the last keyboard/mouse input.
type IdleSource interface {
	IdleSeconds() float64
}

// TitleSource reports the foreground window's title. It is optional: the real
// Win32 source implements it, but a ForegroundSource need not. Callers type-
// assert for it and only capture detail when both the assertion succeeds and
// title capture is enabled (see Agent.captureDetail and the watcher).
type TitleSource interface {
	ForegroundTitle() string
}

// IdleFromTickCount computes idle seconds from GetTickCount-style values,
// safe across uint32 wraparound (~49.7 days of uptime).
func IdleFromTickCount(nowTick, lastInputTick uint32) float64 {
	var delta uint32
	if nowTick >= lastInputTick {
		delta = nowTick - lastInputTick
	} else {
		delta = (0xFFFFFFFF - lastInputTick) + nowTick + 1
	}
	return float64(delta) / 1000.0
}
