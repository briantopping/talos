// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumes

// SetDeviceNumber replaces the device-number lookup for a test and returns a function restoring it.
func SetDeviceNumber(f func(string) (uint32, uint32, error)) func() {
	prev := deviceNumber
	deviceNumber = f

	return func() { deviceNumber = prev }
}
