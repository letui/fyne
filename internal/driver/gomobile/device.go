package gomobile

import (
	"github.com/fyne-io/mobile/event/size"

	"fyne.io/fyne"
)

type device struct {
}

var currentOrientation size.Orientation

// Declare conformity with Device
var _ fyne.Device = (*device)(nil)

func (device) Orientation() fyne.DeviceOrientation {
	switch currentOrientation {
	case size.OrientationLandscape:
		return fyne.OrientationHorizontalLeft
	default:
		return fyne.OrientationVertical
	}
}

func (device) IsMobile() bool {
	return true
}

func (device) HasKeyboard() bool {
	return false
}
