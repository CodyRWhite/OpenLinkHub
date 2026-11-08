package gpuaura

// Package: gpuaura
// Driver for the first-generation ASUS Aura GPU RGB controller found on Pascal-era
// ASUS ROG STRIX / Poseidon GeForce GTX 10-series cards (SMBus address 0x29).
//
// Protocol from OpenRGB's AsusAuraGPUController (GPL-2.0-or-later, Jan Rettig /
// OpenRGB project) and confirmed live on an ASUS ROG STRIX GeForce GTX 1080 Ti
// Gaming OC (PCI 10DE:1B06, subsystem 1043:85EA). Structure follows the gpuene
// driver from the CodyRWhite/OpenLinkHub fork.
//
// Wire protocol (plain SMBus byte-data, chip at i2c address 0x29):
//   identify:  read_byte_data(0x20) == 0x15 && read_byte_data(0x21) == 0x89
//   color:     write_byte_data(0x04, R), (0x05, G), (0x06, B)
//   mode:      write_byte_data(0x07, mode)   1 = static, 4 = spectrum cycle
//
// The whole card is a single RGB zone, so OpenLinkHub renders every effect in
// software and streams the one color with the chip held in static mode (OpenRGB's
// "Direct" mode works the same way). Register 0x0E ("save/apply") commits the
// current state to the controller's non-volatile memory. This driver never
// writes it: OpenRGB treats it as manual-only on these models to avoid flash
// wear, and the card falls back to its stored colors whenever OpenLinkHub isn't
// running.

import (
	"OpenLinkHub/src/common"
	"OpenLinkHub/src/config"
	"OpenLinkHub/src/logger"
	"OpenLinkHub/src/rgb"
	"OpenLinkHub/src/smbus"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	auraAddr      uint8 = 0x29
	regRed        uint8 = 0x04
	regGreen      uint8 = 0x05
	regBlue       uint8 = 0x06
	regMode       uint8 = 0x07
	regMagicHigh  uint8 = 0x20
	regMagicLow   uint8 = 0x21
	magicHigh     uint8 = 0x15
	magicLow      uint8 = 0x89
	modeStatic    uint8 = 0x01
	frameInterval       = 40 * time.Millisecond
)

// supportedCards lists the PCI device/subsystem pairs (vendor 10DE, subsystem
// vendor 1043) that OpenRGB registers for this controller at address 0x29. Only
// 1B06:85EA has been tested with this driver. The rest share the same controller
// according to OpenRGB but are unverified here.
var supportedCards = map[[2]uint16]string{
	{0x1C81, 0x85D8}: "ASUS ROG STRIX GeForce GTX 1050 Gaming OC",
	{0x1C82, 0x8611}: "ASUS ROG STRIX GeForce GTX 1050 Ti Gaming",
	{0x1C82, 0x85CD}: "ASUS ROG STRIX GeForce GTX 1050 Ti Gaming OC",
	{0x1C82, 0x85D1}: "ASUS ROG STRIX GeForce GTX 1050 Ti Gaming OC",
	{0x1C03, 0x85A4}: "ASUS ROG STRIX GeForce GTX 1060 Gaming",
	{0x1C03, 0x85AC}: "ASUS ROG STRIX GeForce GTX 1060 Gaming",
	{0x1C03, 0x865B}: "ASUS ROG STRIX GeForce GTX 1060 Gaming",
	{0x1B81, 0x8598}: "ASUS ROG STRIX GeForce GTX 1070 Gaming",
	{0x1B81, 0x8599}: "ASUS ROG STRIX GeForce GTX 1070 Gaming OC",
	{0x1B81, 0x85A2}: "ASUS ROG STRIX GeForce GTX 1070 OC",
	{0x1B82, 0x861D}: "ASUS ROG STRIX GeForce GTX 1070 Ti Gaming",
	{0x1B82, 0x861E}: "ASUS ROG STRIX GeForce GTX 1070 Ti A8G Gaming",
	{0x1B80, 0x8592}: "ASUS ROG STRIX GeForce GTX 1080 Gaming OC",
	{0x1B80, 0x85AA}: "ASUS ROG STRIX GeForce GTX 1080 A8G Gaming",
	{0x1B80, 0x85F9}: "ASUS ROG STRIX GeForce GTX 1080 Gaming OC",
	{0x1B80, 0x85E8}: "ASUS ROG STRIX GeForce GTX 1080 Gaming OC 11Gbps",
	{0x1B06, 0x85EB}: "ASUS ROG STRIX GeForce GTX 1080 Ti Gaming",
	{0x1B06, 0x85F1}: "ASUS ROG STRIX GeForce GTX 1080 Ti Gaming",
	{0x1B06, 0x85EA}: "ASUS ROG STRIX GeForce GTX 1080 Ti Gaming OC",
	{0x1B06, 0x85E4}: "ASUS ROG STRIX GeForce GTX 1080 Ti Gaming OC",
	{0x1B06, 0x85EC}: "ASUS ROG Poseidon GeForce GTX 1080 Ti",
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
	RGBModes      []string
	activeRgb     *rgb.ActiveRGB
	deviceLock    sync.Mutex
	lastColor     [3]byte
	lastValid     bool
	original      [4]byte // R, G, B, mode as found at startup, restored on Stop
	Exit          bool
	instance      *common.Device
}

var (
	pwd   = ""
	mutex sync.Mutex
)

func readHexFile(path string) (uint16, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// findCandidateBuses returns the /dev/i2c-N paths of NVIDIA i2c adapters whose
// parent PCI device is a supported ASUS card, along with the card name. Nothing
// is sent on the bus here. It only reads sysfs.
func findCandidateBuses() ([]string, string) {
	entries, _ := filepath.Glob("/sys/class/i2c-dev/i2c-*")
	var buses []string
	product := ""
	for _, entry := range entries {
		name, err := os.ReadFile(entry + "/name")
		if err != nil || !strings.HasPrefix(string(name), "NVIDIA i2c adapter") {
			continue
		}
		// <entry>/device -> .../0000:01:00.0/i2c-N, so the PCI device is one level up.
		pciDir, err := filepath.EvalSymlinks(entry + "/device/..")
		if err != nil {
			continue
		}
		vendor, ok1 := readHexFile(pciDir + "/vendor")
		device, ok2 := readHexFile(pciDir + "/device")
		subVendor, ok3 := readHexFile(pciDir + "/subsystem_vendor")
		subDevice, ok4 := readHexFile(pciDir + "/subsystem_device")
		if !ok1 || !ok2 || !ok3 || !ok4 || vendor != 0x10DE || subVendor != 0x1043 {
			continue
		}
		if card, ok := supportedCards[[2]uint16{device, subDevice}]; ok {
			buses = append(buses, "/dev/"+filepath.Base(entry))
			product = card
		}
	}
	return buses, product
}

// probeBus opens a candidate bus and does OpenRGB's read-only magic check.
func probeBus(path string) (*os.File, bool) {
	conn, err := smbus.Open(path)
	if err != nil {
		logger.Log(logger.Fields{"error": err, "bus": path}).Warn("Unable to open i2c bus for ASUS Aura GPU probe")
		return nil, false
	}
	hi, err1 := smbus.ReadRegister(conn.File, auraAddr, regMagicHigh)
	lo, err2 := smbus.ReadRegister(conn.File, auraAddr, regMagicLow)
	if err1 != nil || err2 != nil || hi != magicHigh || lo != magicLow {
		_ = conn.File.Close()
		return nil, false
	}
	return conn.File, true
}

func (d *Device) readReg(reg uint8) (uint8, error) {
	d.deviceLock.Lock()
	defer d.deviceLock.Unlock()
	return smbus.ReadRegister(d.dev, auraAddr, reg)
}

func (d *Device) writeReg(reg, val uint8) error {
	d.deviceLock.Lock()
	defer d.deviceLock.Unlock()
	return smbus.WriteByte(d.dev, auraAddr, reg, val)
}

// Init finds a supported card, verifies the controller, and returns a device
// instance, or nil. Called once at startup from devices.go when enableGpuRGB is set.
func Init() *common.Device {
	pwd = config.GetConfig().ConfigPath

	buses, product := findCandidateBuses()
	var f *os.File
	busPath := ""
	for _, path := range buses {
		if conn, ok := probeBus(path); ok {
			f = conn
			busPath = path
			break
		}
	}
	if f == nil {
		logger.Log(logger.Fields{"candidates": buses}).Warn("No supported ASUS Aura GPU RGB controller found")
		return nil
	}

	d := &Device{
		dev:           f,
		busPath:       busPath,
		Product:       product,
		DeviceVersion: "Aura GPU (0x29)",
		Template:      "gpuaura.html",
		RGBModes:      rgbModes,
		LedCount:      1,
	}
	// Synthesized, alphanumeric-only serial (API deviceId validation rejects dashes).
	d.Serial = "gpuaura" + strings.TrimPrefix(busPath, "/dev/i2c-")

	for i, reg := range []uint8{regRed, regGreen, regBlue, regMode} {
		v, err := d.readReg(reg)
		if err != nil {
			logger.Log(logger.Fields{"error": err, "register": reg}).Error("Unable to read ASUS Aura GPU state")
			_ = f.Close()
			return nil
		}
		d.original[i] = v
	}

	d.RgbDevices = map[int]*RgbDevice{
		0: {ChannelId: 0, LedChannels: d.LedCount, RGB: "static", Label: "GPU"},
	}

	d.loadRgb()
	d.loadDeviceProfiles()
	if d.DeviceProfile == nil {
		d.setDefaults()
		d.saveDeviceProfile()
	} else {
		d.RgbDevices[0].RGB = d.DeviceProfile.RGBProfile
	}

	// Hold the chip in static mode. Every effect is streamed as color writes.
	if err := d.writeReg(regMode, modeStatic); err != nil {
		logger.Log(logger.Fields{"error": err}).Error("Unable to set ASUS Aura GPU static mode")
		_ = f.Close()
		return nil
	}

	d.setDeviceColor()
	d.createDevice()

	logger.Log(logger.Fields{
		"serial":   d.Serial,
		"product":  d.Product,
		"bus":      busPath,
		"original": d.original,
	}).Info("ASUS Aura GPU RGB controller initialized")
	return d.instance
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
		ProductType: common.ProductTypeGpuAura,
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
		logger.Log(logger.Fields{"error": err, "serial": d.Serial}).Warn("Unable to decode gpuaura profile")
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
		logger.Log(logger.Fields{"error": err, "location": d.DeviceProfile.Path}).Error("Unable to save gpuaura device profile")
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
					time.Sleep(frameInterval)
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
				time.Sleep(frameInterval)
			}
		}
	}(int(d.LedCount))
}

// writeLedColors takes the rgb package's flat R,G,B buffer and writes the first
// (only) LED to the controller, skipping the bus entirely when nothing changed.
func (d *Device) writeLedColors(buff []byte) {
	if len(buff) < 3 {
		return
	}
	c := [3]byte{buff[0], buff[1], buff[2]}
	if d.lastValid && c == d.lastColor {
		return
	}
	for i, reg := range []uint8{regRed, regGreen, regBlue} {
		if err := d.writeReg(reg, c[i]); err != nil {
			logger.Log(logger.Fields{"error": err, "register": reg}).Warn("Unable to write ASUS Aura GPU color")
			d.lastValid = false
			return
		}
	}
	d.lastColor = c
	d.lastValid = true
}

// Stop ends any running effect and restores the colors and mode the card had when
// OpenLinkHub started (no save to the card's memory is ever issued).
func (d *Device) Stop() {
	d.Exit = true
	if d.activeRgb != nil {
		d.activeRgb.Exit <- true
		d.activeRgb = nil
	}
	time.Sleep(2 * frameInterval)
	if d.dev != nil {
		_ = d.writeReg(regRed, d.original[0])
		_ = d.writeReg(regGreen, d.original[1])
		_ = d.writeReg(regBlue, d.original[2])
		_ = d.writeReg(regMode, d.original[3])
		_ = d.dev.Close()
	}
}
