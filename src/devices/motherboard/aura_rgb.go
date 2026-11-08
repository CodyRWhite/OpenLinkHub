package motherboard

// ASUS Aura USB motherboard RGB (0b05:19af, ITE-based) support for the motherboard device.
//
// Kept inside the motherboard package (not a separate device) so the board shows up as one
// entry in the UI, with fan headers and RGB together.
//
// Hardware facts confirmed by live testing on the ROG STRIX Z590-I GAMING WIFI:
//   - The ROG logo is Aura device byte 0x01 with 4 LEDs. It is NOT the "FIXED" zone (0x04)
//     that OpenRGB's generic config-table logic implies; writes there do nothing visible.
//   - The chip persists its last committed effect mode across reboots, and while it is in a
//     hardware effect mode it silently drops direct (0x40) writes, even though every write is
//     ACKed at the USB level. Init therefore always asserts Direct mode (0x35, mode 0xFF) on
//     effect channels 0-2 and commits (0x3F 0x55), which makes it self-healing.
//   - The Gen2 ARGB headers (chassis + CPU) do not respond to this protocol at all.
//
// Wire protocol (65-byte HID reports, report ID 0xEC), as used by OpenRGB's AuraUSB driver:
//   0x82            -> read firmware string (reply[1] == 0x02, string at reply[2:18])
//   0xB0            -> read config table   (reply[1] == 0x30)
//   0x35 ch 0 0 m   -> set effect mode m on effect channel ch (0xFF = direct)
//   0x3F 0x55       -> commit
//   0x40 d s n rgb… -> direct colors for device byte d (|0x80 = apply), start LED s, n LEDs

import (
	"OpenLinkHub/src/common"
	"OpenLinkHub/src/logger"
	"OpenLinkHub/src/rgb"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/sstallion/go-hid"
)

const (
	auraVendorId       = 0x0b05
	auraProductId      = 0x19af
	auraReportId       = 0xec
	auraReportSize     = 65
	auraCmdFirmware    = 0x82
	auraCmdConfigTable = 0xb0
	auraCmdSetEffect   = 0x35
	auraCmdCommit      = 0x3f
	auraCmdDirect      = 0x40
	auraCmdControl     = 0x52
	auraModeDirect     = 0xff
	auraLogoDevice     = 0x01 // ROG logo (confirmed by live testing, see header comment)
	auraLogoLedCount   = 4
	auraDirectApply    = 0x80
	auraMaxLedsPerPkt  = 20
)

var auraRgbModes = []string{
	"arc", "circle", "circleshift", "colorpulse", "colorshift", "colorwarp",
	"flickering", "gradient", "marquee", "nebula", "off", "pastelrainbow",
	"pastelspiralrainbow", "rain", "rainbow", "rotarystack", "rotator",
	"sequential", "spiralrainbow", "spinner", "static", "storm", "watercolor", "wave",
}

var auraRgbProfileUpgrade = []string{
	"arc", "nebula", "marquee", "rotarystack", "sequential", "spiralrainbow",
	"gradient", "pastelrainbow", "pastelspiralrainbow", "rain",
}

type RGBOverride struct {
	Enabled        bool
	RGBStartColor  rgb.Color
	RGBEndColor    rgb.Color
	RGBMiddleColor rgb.Color
	RgbModeSpeed   float64
}

type RgbDevice struct {
	ChannelId   int
	LedChannels uint8
	RGB         string
	Label       string
}

// defaultRgbOverride is the per-channel override template every OpenLinkHub device starts with.
func defaultRgbOverride() map[int]map[int]RGBOverride {
	return map[int]map[int]RGBOverride{
		0: {
			0: {
				Enabled: false,
				RGBStartColor: rgb.Color{
					Red: 0, Green: 255, Blue: 255, Brightness: 1, Hex: "#00ffff", Temperature: 20,
				},
				RGBMiddleColor: rgb.Color{
					Red: 0, Green: 255, Blue: 255, Brightness: 1, Hex: "#00ffff", Temperature: 40,
				},
				RGBEndColor: rgb.Color{
					Red: 0, Green: 255, Blue: 255, Brightness: 1, Hex: "#00ffff", Temperature: 60,
				},
				RgbModeSpeed: 3,
			},
		},
	}
}

// initAuraRgb opens the Aura USB controller, forces it into Direct mode and starts the
// saved RGB profile. Missing hardware is not an error: the fan headers keep working.
func (d *Device) initAuraRgb() {
	dev, err := hid.OpenFirst(auraVendorId, auraProductId)
	if err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("No ASUS Aura USB RGB controller found; motherboard RGB will be unavailable")
		return
	}

	d.auraDev = dev
	d.RGBModes = auraRgbModes

	if resp, err := d.auraTransfer(auraCmdFirmware, nil, true); err == nil && len(resp) > 17 && resp[1] == 0x02 {
		d.Firmware = strings.TrimRight(string(resp[2:18]), "\x00")
	}

	// The config table is only used as a sanity check that this really is an Aura controller.
	// Its zone layout is wrong for this board (see header comment), so the logo is hardcoded.
	cfg, err := d.auraTransfer(auraCmdConfigTable, nil, true)
	if err != nil || len(cfg) < 64 || cfg[1] != 0x30 {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to read ASUS Aura config table")
		_ = d.auraDev.Close()
		d.auraDev = nil
		return
	}

	d.LedCount = auraLogoLedCount

	rgbProfile := "static"
	label := "Motherboard"
	if d.DeviceProfile != nil {
		if len(d.DeviceProfile.RGBLabel) > 0 {
			label = d.DeviceProfile.RGBLabel
		}
		if len(d.DeviceProfile.RGBProfile) > 0 {
			rgbProfile = d.DeviceProfile.RGBProfile
		}
		if d.DeviceProfile.BrightnessSlider == nil {
			brightness := uint8(100)
			d.DeviceProfile.BrightnessSlider = &brightness
			d.DeviceProfile.Brightness = 1
		}
		if d.DeviceProfile.RGBOverride == nil {
			d.DeviceProfile.RGBOverride = map[int]map[int]RGBOverride{0: {0: {RgbModeSpeed: 3}}}
		}
	}

	d.RgbDevices = map[int]*RgbDevice{
		0: {
			ChannelId:   0,
			LedChannels: d.LedCount,
			RGB:         rgbProfile,
			Label:       label,
		},
	}

	d.loadRgb()

	// Assert software control and Direct mode on every effect channel, then commit. The chip
	// remembers its last committed mode across reboots and silently ignores direct writes
	// while in a hardware effect, so this must run on every init.
	_, _ = d.auraTransfer(auraCmdControl, []byte{0x53, 0x00, 0x01}, false)
	for _, channel := range []byte{0x00, 0x01, 0x02} {
		_, _ = d.auraTransfer(auraCmdSetEffect, []byte{channel, 0x00, 0x00, auraModeDirect}, false)
	}
	_, _ = d.auraTransfer(auraCmdCommit, []byte{0x55}, false)

	d.setDeviceColor()

	logger.Log(logger.Fields{"serial": d.Serial, "firmware": d.Firmware, "ledCount": d.LedCount}).Info("ASUS Aura USB RGB controller initialized")
}

// stopAuraRgb stops any running effect and releases the Aura controller.
func (d *Device) stopAuraRgb() {
	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}

	if d.auraDev != nil {
		_ = d.auraDev.Close()
		d.auraDev = nil
	}
}

// auraTransfer sends one 65-byte report (0xEC, cmd, args...) and optionally reads the reply.
func (d *Device) auraTransfer(cmd byte, args []byte, readResponse bool) ([]byte, error) {
	d.auraDevLock.Lock()
	defer d.auraDevLock.Unlock()

	buf := make([]byte, auraReportSize)
	buf[0] = auraReportId
	buf[1] = cmd
	copy(buf[2:], args)

	if _, err := d.auraDev.Write(buf); err != nil {
		return nil, err
	}

	if !readResponse {
		return nil, nil
	}

	resp := make([]byte, auraReportSize)
	if _, err := d.auraDev.Read(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// auraSendDirect writes R,G,B triplets to the logo in packets of up to 20 LEDs, setting the
// apply bit on the last packet.
func (d *Device) auraSendDirect(colors []byte) error {
	total := len(colors) / 3
	for start := 0; start < total; start += auraMaxLedsPerPkt {
		end := start + auraMaxLedsPerPkt
		count := auraMaxLedsPerPkt
		if end > total {
			count = total - start
		}

		device := byte(auraLogoDevice)
		if end >= total {
			device |= auraDirectApply
		}

		pkt := make([]byte, 3+count*3)
		pkt[0] = device
		pkt[1] = byte(start)
		pkt[2] = byte(count)
		copy(pkt[3:], colors[start*3:(start+count)*3])

		if _, err := d.auraTransfer(auraCmdDirect, pkt, false); err != nil {
			return err
		}
	}
	return nil
}

// writeLedColors takes a flat R,G,B,R,G,B... buffer and writes it to the logo.
func (d *Device) writeLedColors(buff []byte) {
	if d.auraDev == nil {
		return
	}
	if err := d.auraSendDirect(buff); err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to write Aura motherboard LED colors")
	}
}

// loadRgb will load the RGB profile file if found, or create the default.
func (d *Device) loadRgb() {
	rgbDirectory := pwd + "/database/rgb/"
	rgbFilename := rgbDirectory + d.Serial + ".json"

	if !common.IsValidExtension(rgbFilename, ".json") {
		return
	}

	if !common.FileExists(rgbFilename) {
		profile := rgb.GetRGB()
		profile.Device = d.Product
		if err := common.SaveJsonData(rgbFilename, profile); err != nil {
			logger.Log(logger.Fields{"error": err, "location": rgbFilename}).Error("Unable to write rgb profile data")
			return
		}
	}

	file, err := os.Open(rgbFilename)
	if err != nil {
		logger.Log(logger.Fields{"error": err, "serial": d.Serial, "location": rgbFilename}).Warn("Unable to load RGB")
		return
	}
	if err = json.NewDecoder(file).Decode(&d.Rgb); err != nil {
		logger.Log(logger.Fields{"error": err, "serial": d.Serial, "location": rgbFilename}).Warn("Unable to decode profile")
	}
	_ = file.Close()

	d.upgradeRgbProfile(rgbFilename)
}

// upgradeRgbProfile adds RGB profiles missing from an older profile file and refreshes
// outdated ones.
func (d *Device) upgradeRgbProfile(path string) {
	if d.Rgb == nil {
		return
	}
	save := false
	for _, profile := range auraRgbProfileUpgrade {
		if d.GetRgbProfile(profile) == nil {
			save = true
			template := rgb.GetRgbProfile(profile)
			if template == nil {
				d.Rgb.Profiles[profile] = rgb.Profile{}
			} else {
				d.Rgb.Profiles[profile] = *template
			}
		}
	}
	for key, val := range d.Rgb.Profiles {
		template := rgb.GetRgbProfile(key)
		if template == nil {
			continue
		}
		if val.Version != template.Version {
			d.Rgb.Profiles[key] = *template
			save = true
		}
	}
	if save {
		_ = common.SaveJsonData(path, d.Rgb)
	}
}

// GetRgbProfile returns one RGB profile by name.
func (d *Device) GetRgbProfile(profile string) *rgb.Profile {
	if d.Rgb == nil {
		return nil
	}
	if val, ok := d.Rgb.Profiles[profile]; ok {
		return &val
	}
	return nil
}

// GetRgbProfiles returns the RGB profiles this device supports.
func (d *Device) GetRgbProfiles() interface{} {
	if d.Rgb == nil {
		return nil
	}
	tmp := *d.Rgb
	profiles := make(map[string]rgb.Profile, len(tmp.Profiles))
	for key, value := range tmp.Profiles {
		for _, m := range auraRgbModes {
			if m == key {
				profiles[key] = value
				break
			}
		}
	}
	tmp.Profiles = profiles
	return tmp
}

// ProcessGetRgbOverride returns the RGB override for a channel (used by the override dialog).
func (d *Device) ProcessGetRgbOverride(channelId, subDeviceId int) interface{} {
	return d.getRgbOverride(channelId, subDeviceId)
}

func (d *Device) getRgbOverride(channelId, subDeviceId int) *RGBOverride {
	if d.DeviceProfile == nil {
		return nil
	}
	if value, ok := d.DeviceProfile.RGBOverride[channelId]; ok {
		if val, found := value[subDeviceId]; found {
			return &val
		}
	}
	return nil
}

// UpdateRgbProfile assigns an RGB profile to the logo.
func (d *Device) UpdateRgbProfile(_ int, profile string) uint8 {
	if d.DeviceProfile == nil || d.RgbDevices == nil || d.RgbDevices[0] == nil {
		return 0
	}
	if d.GetRgbProfile(profile) == nil {
		logger.Log(logger.Fields{"serial": d.Serial, "profile": profile}).Warn("Non-existing RGB profile")
		return 0
	}
	d.RgbDevices[0].RGB = profile
	d.saveDeviceProfile()

	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	d.setDeviceColor()
	return 1
}

// ProcessSetRgbOverride updates the RGB override for a channel.
func (d *Device) ProcessSetRgbOverride(channelId, subDeviceId int, enabled bool, startColor, endColor, middleColor rgb.Color, speed float64) uint8 {
	if d.DeviceProfile == nil {
		return 0
	}
	if speed < 0 || speed > 10 {
		return 0
	}
	rgbOverride := d.getRgbOverride(channelId, subDeviceId)
	if rgbOverride == nil {
		return 0
	}
	rgbOverride.Enabled = enabled
	rgbOverride.RGBStartColor = startColor
	rgbOverride.RGBEndColor = endColor
	rgbOverride.RGBMiddleColor = middleColor
	rgbOverride.RgbModeSpeed = speed

	if value, ok := d.DeviceProfile.RGBOverride[channelId]; ok {
		value[subDeviceId] = *rgbOverride
		d.DeviceProfile.RGBOverride[channelId] = value
	}
	d.saveDeviceProfile()

	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	d.setDeviceColor()
	return 1
}

// ChangeDeviceBrightness sets the brightness mode (0-3).
func (d *Device) ChangeDeviceBrightness(mode uint8) uint8 {
	if d.DeviceProfile == nil {
		return 0
	}
	d.DeviceProfile.Brightness = mode
	d.saveDeviceProfile()
	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	d.setDeviceColor()
	return 1
}

// ChangeDeviceBrightnessValue sets the brightness slider (0-100).
func (d *Device) ChangeDeviceBrightnessValue(value uint8) uint8 {
	if value > 100 || d.DeviceProfile == nil {
		return 0
	}
	d.DeviceProfile.BrightnessSlider = &value
	d.saveDeviceProfile()
	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	d.setDeviceColor()
	return 1
}

// setDeviceColor renders the current profile. Static and off are written once; anything
// else starts a render goroutine, the same pattern every other OpenLinkHub device uses.
func (d *Device) setDeviceColor() {
	if d.RgbDevices == nil || d.RgbDevices[0] == nil || d.RgbDevices[0].LedChannels == 0 {
		return
	}

	if d.DeviceProfile != nil && d.DeviceProfile.RgbOff {
		d.writeLedColors(make([]byte, int(d.LedCount)*3))
		return
	}

	rgbName := d.RgbDevices[0].RGB

	if rgbName == "off" {
		d.writeLedColors(make([]byte, int(d.LedCount)*3))
		return
	}

	if rgbName == "static" {
		profile := d.GetRgbProfile("static")
		if profile == nil {
			return
		}
		var c *rgb.Color
		rgbOverride := d.getRgbOverride(0, 0)
		if rgbOverride != nil && rgbOverride.Enabled {
			c = rgb.ModifyBrightness(rgbOverride.RGBStartColor)
		} else {
			if d.DeviceProfile != nil && d.DeviceProfile.BrightnessSlider != nil {
				profile.StartColor.Brightness = rgb.GetBrightnessValueFloat(*d.DeviceProfile.BrightnessSlider)
			}
			c = rgb.ModifyBrightness(profile.StartColor)
		}
		buff := make([]byte, 0, int(d.LedCount)*3)
		for i := 0; i < int(d.LedCount); i++ {
			buff = append(buff, byte(c.Red), byte(c.Green), byte(c.Blue))
		}
		d.writeLedColors(buff)
		return
	}

	go func(ledCount int) {
		startTime := time.Now()
		d.activeRgb = rgb.Exit()
		d.activeRgb.RGBStartColor = rgb.GenerateRandomColor(1)
		d.activeRgb.RGBEndColor = rgb.GenerateRandomColor(1)

		for {
			select {
			case <-d.activeRgb.Exit:
				return
			default:
				profile := d.GetRgbProfile(rgbName)
				if profile == nil {
					d.writeLedColors(make([]byte, ledCount*3))
					time.Sleep(20 * time.Millisecond)
					continue
				}

				rgbCustomColor := true
				if (rgb.Color{}) == profile.StartColor || (rgb.Color{}) == profile.EndColor {
					rgbCustomColor = false
				}
				rgbModeSpeed := common.FClamp(profile.Speed, 0.1, 10)

				r := rgb.New(
					ledCount,
					rgbModeSpeed,
					nil,
					nil,
					profile.Brightness,
					common.Clamp(profile.Smoothness, 1, 100),
					time.Duration(rgbModeSpeed)*time.Second,
					rgbCustomColor,
				)

				if rgbCustomColor {
					r.RGBStartColor = &profile.StartColor
					r.RGBEndColor = &profile.EndColor
					r.RGBMiddleColor = &profile.MiddleColor
				} else {
					r.RGBStartColor = d.activeRgb.RGBStartColor
					r.RGBEndColor = d.activeRgb.RGBEndColor
					r.RGBMiddleColor = d.activeRgb.RGBMiddleColor
				}
				if r.RGBMiddleColor == nil {
					r.RGBMiddleColor = &rgb.Color{}
				}

				rgbOverride := d.getRgbOverride(0, 0)
				if rgbOverride != nil && rgbOverride.Enabled {
					r.RGBStartColor = &rgbOverride.RGBStartColor
					r.RGBEndColor = &rgbOverride.RGBEndColor
					r.RGBMiddleColor = &rgbOverride.RGBMiddleColor
					r.RgbModeSpeed = common.FClamp(rgbOverride.RgbModeSpeed, 0.1, 10)
				}

				if d.DeviceProfile != nil && d.DeviceProfile.BrightnessSlider != nil {
					r.RGBBrightness = rgb.GetBrightnessValueFloat(*d.DeviceProfile.BrightnessSlider)
				}
				r.RGBStartColor.Brightness = r.RGBBrightness
				r.RGBEndColor.Brightness = r.RGBBrightness
				r.RGBMiddleColor.Brightness = r.RGBBrightness

				switch rgbName {
				case "rainbow":
					r.Rainbow(startTime)
				case "pastelrainbow":
					r.PastelRainbow(startTime)
				case "spiralrainbow":
					r.SpiralRainbow(startTime)
				case "pastelspiralrainbow":
					r.PastelSpiralRainbow(startTime)
				case "arc":
					r.Arc(startTime)
				case "rain":
					r.Rain(startTime)
				case "watercolor":
					r.Watercolor(startTime)
				case "gradient":
					r.ColorshiftGradient(startTime, profile.Gradients, profile.Speed)
				case "colorpulse":
					r.Colorpulse(&startTime)
				case "rotator":
					r.Rotator(&startTime)
				case "wave":
					r.Wave(&startTime)
				case "storm":
					r.Storm()
				case "flickering":
					r.Flickering(&startTime)
				case "colorshift":
					r.Colorshift(&startTime, d.activeRgb)
				case "circleshift":
					r.CircleShift(&startTime)
				case "circle":
					r.Circle(&startTime)
				case "spinner":
					r.Spinner(&startTime)
				case "colorwarp":
					r.Colorwarp(&startTime, d.activeRgb)
				case "nebula":
					r.Nebula(&startTime)
				case "marquee":
					r.Marquee(&startTime)
				case "rotarystack":
					r.RotaryStack(&startTime)
				default:
					r.Sequential(&startTime)
				}

				d.writeLedColors(r.Output)
				time.Sleep(20 * time.Millisecond)
			}
		}
	}(int(d.LedCount))
}
