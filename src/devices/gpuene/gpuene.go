package gpuene

// Package: gpuene
// Driver for ASUS "ENE SMBus" GPU RGB controllers (2nd-gen, device string family
// AUMA0-E6K5-0107 and its known-equal siblings 1107/1110/1111/1113/1114).
//
// Protocol reverse-engineered from OpenRGB's ENESMBusController (GPL-2.0-or-later,
// Adam Honse / CalcProgrammer1) and confirmed live on an ASUS ROG STRIX LC RTX
// 3080 Ti (PCI subsystem 1043:880A -- one digit off OpenRGB's registered 1043:8809,
// which is why upstream's own PCI-ID-gated detector never reaches this card even
// though the chip and register map are identical). This driver talks to the chip
// directly by device string, not PCI ID, so that gap doesn't matter here.
//
// Wire protocol (SMBus, chip at i2c address 0x67):
//   select 16-bit ENE register: write_word_data(0x00, byteswap16(reg))
//   read a byte:                read_byte_data(0x81)
//   write a byte:                write_byte_data(0x01, val)
//   write a block:               write_block_data(0x03, [len, data...])
// Per-LED direct-mode colors are stored R,B,G (not R,G,B) at
// ENE_REG_COLORS_DIRECT_V2 + 3*led, 3 bytes per LED.

import (
	"OpenLinkHub/src/common"
	"OpenLinkHub/src/config"
	"OpenLinkHub/src/logger"
	"OpenLinkHub/src/rgb"
	"OpenLinkHub/src/smbus"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	eneAddr              uint8  = 0x67
	regDeviceName        uint16 = 0x1000
	regConfigTable       uint16 = 0x1C00
	regColorsDirectV2    uint16 = 0x8100
	regDirect            uint16 = 0x8020
	regMode              uint16 = 0x8021
	regSpeed             uint16 = 0x8022
	regDirection         uint16 = 0x8023
	regApply             uint16 = 0x80A0
	applyVal             byte   = 0x01
	configLedCountOffset        = 0x03 // ENE_CONFIG_LED_COUNT_0107, same offset across the whole -0107 chip family
)

// deviceString -> true for every ENE GPU controller string known (from OpenRGB) to
// use the same V2 direct-color register layout and LED-count offset as the one
// verified live on this card. Only "AUMA0-E6K5-0107" has actually been tested here;
// the rest are included because upstream documents them as register-identical, but
// treat them as unverified on this specific driver.
var supportedDeviceStrings = []string{
	"AUMA0-E6K5-0107",
	"AUMA0-E6K5-1107",
	"AUMA0-E6K5-1110",
	"AUMA0-E6K5-1111",
	"AUMA0-E6K5-1113",
	"AUMA0-E6K5-1114",
}

var rgbModes = []string{
	"arc", "circle", "circleshift", "colorpulse", "colorshift", "colorwarp",
	"flickering", "gradient", "marquee", "nebula", "off", "pastelrainbow",
	"pastelspiralrainbow", "rain", "rainbow", "rotarystack", "rotator",
	"sequential", "spiralrainbow", "spinner", "static", "storm", "watercolor", "wave",
}

var rgbProfileUpgrade = []string{
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

type DeviceProfile struct {
	Active           bool
	Path             string
	Product          string
	Serial           string
	Brightness       uint8
	BrightnessSlider *uint8
	RGBProfile       string
	RGBLabel         string
	RGBOverride      map[int]map[int]RGBOverride
	RgbOff           bool
}

// RgbDevice mirrors the single-zone shape other OpenLinkHub devices expose per
// channel, kept as a map[int] for compatibility with the shared getRgbOverride/
// ProcessSetRgbOverride(channelId, ...) calling convention even though this
// device only ever has one channel (0).
type RgbDevice struct {
	ChannelId   int
	LedChannels uint8
	RGB         string
	Label       string
}

type Device struct {
	Debug         bool
	dev           *os.File
	busPath       string
	Product       string `json:"product"`
	Serial        string `json:"serial"`
	Firmware      string `json:"firmware"`
	DeviceVersion string `json:"deviceVersion"`
	LedCount      uint8
	RgbDevices    map[int]*RgbDevice `json:"rgbDevices"`
	UserProfiles  map[string]*DeviceProfile
	DeviceProfile *DeviceProfile
	Rgb           *rgb.RGB
	Template      string
	activeRgb     *rgb.ActiveRGB
	deviceLock    sync.Mutex
	Exit          bool
	instance      *common.Device
}

var (
	pwd     = ""
	mutex   sync.Mutex
	rgbOnce sync.Once
)

func byteswap16(v uint16) uint16 {
	return ((v << 8) & 0xFF00) | ((v >> 8) & 0x00FF)
}

// eneRead reads one byte from a 16-bit ENE register.
func (d *Device) eneRead(reg uint16) (byte, error) {
	d.deviceLock.Lock()
	defer d.deviceLock.Unlock()

	if err := smbus.WriteWord(d.dev, eneAddr, 0x00, byteswap16(reg)); err != nil {
		return 0, err
	}
	return smbus.ReadRegister(d.dev, eneAddr, 0x81)
}

// eneWriteByte writes one byte to a 16-bit ENE register.
func (d *Device) eneWriteByte(reg uint16, val byte) error {
	d.deviceLock.Lock()
	defer d.deviceLock.Unlock()

	if err := smbus.WriteWord(d.dev, eneAddr, 0x00, byteswap16(reg)); err != nil {
		return err
	}
	return smbus.WriteByte(d.dev, eneAddr, 0x01, val)
}

// eneWriteBlock writes up to 3 bytes to a 16-bit ENE register (the i2c/smbus
// transport this chip uses caps block writes at 3 bytes -- matches OpenRGB's
// ENESMBusInterface_i2c_smbus::GetMaxBlock()).
func (d *Device) eneWriteBlock(reg uint16, data []byte) error {
	d.deviceLock.Lock()
	defer d.deviceLock.Unlock()

	if err := smbus.WriteWord(d.dev, eneAddr, 0x00, byteswap16(reg)); err != nil {
		return err
	}
	return smbus.WriteBlockDataUncached(d.dev, eneAddr, 0x03, data)
}

// probeBus opens one i2c-dev path and checks for a known ENE GPU device string at
// address 0x67. Returns the 16-byte device version string (trimmed) on success.
func probeBus(path string) (string, *os.File, bool) {
	conn, err := smbus.Open(path)
	if err != nil {
		return "", nil, false
	}
	f := conn.File

	chars := make([]byte, 16)
	ok := true
	for i := 0; i < 16; i++ {
		if err := smbusWriteWordRaw(f, byteswap16(regDeviceName+uint16(i))); err != nil {
			ok = false
			break
		}
		b, err := smbus.ReadRegister(f, eneAddr, 0x81)
		if err != nil {
			ok = false
			break
		}
		chars[i] = b
	}
	if !ok {
		_ = f.Close()
		return "", nil, false
	}

	name := strings.TrimRight(string(chars), "\x00")
	for _, known := range supportedDeviceStrings {
		if name == known {
			return name, f, true
		}
	}
	_ = f.Close()
	return "", nil, false
}

// smbusWriteWordRaw is the register-select step, used before the interface's
// deviceLock exists yet (probing happens before a Device is constructed).
func smbusWriteWordRaw(f *os.File, value uint16) error {
	return smbus.WriteWord(f, eneAddr, 0x00, value)
}

// Init will probe for a supported ENE GPU RGB controller and return a device
// instance, or nil if none is found. Unlike USB devices, this has no hotplug
// event to key off of -- it is only ever called once at startup from devices.go,
// gated behind the enableGpuRGB config flag.
func Init() *common.Device {
	pwd = config.GetConfig().ConfigPath

	var busPath string
	var f *os.File
	var deviceVersion string
	for i := 0; i < 8; i++ {
		path := "/dev/i2c-" + itoa(i)
		if name, conn, ok := probeBus(path); ok {
			busPath = path
			f = conn
			deviceVersion = name
			break
		}
	}
	if f == nil {
		logger.Log(logger.Fields{}).Warn("No supported ENE GPU RGB controller found")
		return nil
	}

	d := &Device{
		dev:           f,
		busPath:       busPath,
		DeviceVersion: deviceVersion,
		Product:       "GPU RGB (ENE)",
		Template:      "gpuene.html",
	}

	// Serial is synthesized (this chip has no unique ID we can read) -- stable
	// across restarts as long as it stays on the same i2c bus, which it will
	// unless the GPU is physically moved to a different PCIe slot. Must be pure
	// alphanumeric: common.AlphanumericRegex (used to validate deviceId on every
	// RGB/override API route) rejects dashes, so a serial like "gpuene--dev-i2c-0"
	// would register fine but silently fail every API call as "Non-existing device".
	d.Serial = "gpuene" + strings.TrimPrefix(busPath, "/dev/i2c-")

	configTable := make([]byte, 64)
	for i := 0; i < 64; i++ {
		b, err := d.eneRead(regConfigTable + uint16(i))
		if err != nil {
			logger.Log(logger.Fields{"error": err}).Error("Unable to read ENE config table")
			return nil
		}
		configTable[i] = b
	}
	d.LedCount = configTable[configLedCountOffset]
	if d.LedCount == 0 || d.LedCount > 64 {
		logger.Log(logger.Fields{"ledCount": d.LedCount}).Error("ENE GPU controller reported implausible LED count")
		return nil
	}

	d.RgbDevices = map[int]*RgbDevice{
		0: {ChannelId: 0, LedChannels: d.LedCount, RGB: "static", Label: "GPU"},
	}

	d.loadRgb()
	d.loadDeviceProfiles()
	d.getDeviceProfile()
	if d.DeviceProfile == nil {
		d.setDefaults()
		d.saveDeviceProfile()
	} else {
		d.RgbDevices[0].RGB = d.DeviceProfile.RGBProfile
	}

	// Switch the chip into direct-color mode once at startup.
	_ = d.eneWriteByte(regDirect, 1)
	_ = d.eneWriteByte(regApply, applyVal)

	d.setDeviceColor()
	d.createDevice()

	logger.Log(logger.Fields{"serial": d.Serial, "deviceVersion": d.DeviceVersion, "ledCount": d.LedCount, "bus": busPath}).Info("ENE GPU RGB controller initialized")
	return d.instance
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func (d *Device) setDefaults() {
	brightness := uint8(100)
	d.DeviceProfile = &DeviceProfile{
		Active:           true,
		Path:             pwd + "/database/profiles/" + d.Serial + ".json",
		Product:          d.Product,
		Serial:           d.Serial,
		Brightness:       1,
		BrightnessSlider: &brightness,
		RGBProfile:       "static",
		RGBLabel:         "GPU",
		RGBOverride: map[int]map[int]RGBOverride{
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
		},
	}
}

// loadRgb will load the RGB profile file if found, or create the default -- same
// per-serial-file convention every other OpenLinkHub device uses.
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

func (d *Device) upgradeRgbProfile(path string) {
	save := false
	for _, profile := range rgbProfileUpgrade {
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

func (d *Device) GetRgbProfile(profile string) *rgb.Profile {
	if d.Rgb == nil {
		return nil
	}
	if val, ok := d.Rgb.Profiles[profile]; ok {
		return &val
	}
	return nil
}

func (d *Device) GetRgbProfiles() interface{} {
	if d.Rgb == nil {
		return nil
	}
	tmp := *d.Rgb
	profiles := make(map[string]rgb.Profile, len(tmp.Profiles))
	for key, value := range tmp.Profiles {
		for _, m := range rgbModes {
			if m == key {
				profiles[key] = value
				break
			}
		}
	}
	tmp.Profiles = profiles
	return tmp
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

func (d *Device) GetDeviceTemplate() string {
	return d.Template
}

func (d *Device) createDevice() {
	d.instance = &common.Device{
		ProductType: common.ProductTypeGpuEne,
		Product:     d.Product,
		Serial:      d.Serial,
		Firmware:    d.DeviceVersion,
		Image:       "icon-device.svg",
		Instance:    d,
		GetDevice:   d,
	}
}

func (d *Device) loadDeviceProfiles() {
	profilePath := pwd + "/database/profiles/" + d.Serial + ".json"
	if !common.FileExists(profilePath) {
		return
	}
	file, err := os.Open(profilePath)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	pf := &DeviceProfile{}
	if err = json.NewDecoder(file).Decode(pf); err != nil {
		logger.Log(logger.Fields{"error": err, "serial": d.Serial}).Warn("Unable to decode gpuene profile")
		return
	}
	if pf.Serial == d.Serial {
		d.DeviceProfile = pf
	}
}

func (d *Device) getDeviceProfile() {
	// loadDeviceProfiles already sets d.DeviceProfile directly for this
	// single-profile-per-device driver (no named-profile switching yet).
}

func (d *Device) saveDeviceProfile() {
	mutex.Lock()
	defer mutex.Unlock()

	if d.DeviceProfile == nil {
		return
	}
	d.DeviceProfile.Path = pwd + "/database/profiles/" + d.Serial + ".json"
	d.DeviceProfile.Product = d.Product
	d.DeviceProfile.Serial = d.Serial
	d.DeviceProfile.RGBProfile = d.RgbDevices[0].RGB

	if err := common.SaveJsonData(d.DeviceProfile.Path, d.DeviceProfile); err != nil {
		logger.Log(logger.Fields{"error": err, "location": d.DeviceProfile.Path}).Error("Unable to save gpuene device profile")
	}
}

// UpdateRgbProfile assigns a profile (e.g. "static", "rainbow", "off") to the
// single GPU channel (channelId is always 0 for this device, kept as a parameter
// only to match the shared reflection-called signature other devices use).
func (d *Device) UpdateRgbProfile(_ int, profile string) uint8 {
	if d.DeviceProfile == nil {
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

// writeLedColors takes a flat R,G,B,R,G,B... buffer (the rgb package's native
// output order) and writes it out to the chip, reordering to the R,B,G wire
// order this chip's direct-color registers actually use.
func (d *Device) writeLedColors(buff []byte) {
	ledCount := len(buff) / 3
	for led := 0; led < ledCount; led++ {
		r := buff[led*3+0]
		g := buff[led*3+1]
		b := buff[led*3+2]
		if err := d.eneWriteBlock(regColorsDirectV2+uint16(3*led), []byte{r, b, g}); err != nil {
			logger.Log(logger.Fields{"error": err, "led": led}).Warn("Unable to write ENE LED color")
		}
	}
}

// setDeviceColor renders the current profile for the single GPU channel. Static
// and off profiles are written once; anything else starts a continuous-render
// goroutine, matching the pattern every other OpenLinkHub device uses.
func (d *Device) setDeviceColor() {
	if d.RgbDevices[0].LedChannels == 0 {
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
			profile.StartColor.Brightness = rgb.GetBrightnessValueFloat(*d.DeviceProfile.BrightnessSlider)
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

				r.RGBBrightness = rgb.GetBrightnessValueFloat(*d.DeviceProfile.BrightnessSlider)
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

func (d *Device) Stop() {
	d.Exit = true
	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	if d.dev != nil {
		_ = d.dev.Close()
	}
}
