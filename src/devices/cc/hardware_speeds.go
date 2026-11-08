package cc

// Hardware ("power-on") fan and pump speed configuration of the iCUE Commander CORE.
//
// The Commander CORE keeps a speed configuration that it runs whenever no software is in
// control (at power-on, and after OpenLinkHub switches it back to hardware mode on stop).
// It lives in three endpoints, documented by liquidctl (driver/commander_core.py and
// docs/developer/protocol/commander_core.md):
//
//	0x60 0x6d  hardware speed mode   data type 03 00  [N][mode per channel]  0x00 fixed %, 0x02 curve %
//	0x61 0x6d  hardware fixed %      data type 04 00  [N][u16 LE percent per channel]
//	0x62 0x6d  hardware curve %      data type 05 00  [N] then per channel [sensor][points][points x (u16 LE temp*10, u16 LE %)]
//
// Channel numbering is the same as OpenLinkHub's ChannelId: 0 = pump, 1-6 = fans.
//
// Stage 1 (always on): read and log all three tables at init, and keep a one-time baseline.
// Stage 2 (hardwareSpeedSaveEnabled, off by default): store the speeds OpenLinkHub is running
// as fixed percentages, on Save and on Stop. Read-modify-write only, then read back to verify.
//
// Whether these settings survive a full power-off is not documented anywhere and must be tested.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"OpenLinkHub/src/common"
	"OpenLinkHub/src/logger"
)

const (
	// hardwareSpeedSaveEnabled turns on the stage 2 writes (Save button and Stop). Keep it off
	// until the stage 1 readout has been checked on real hardware.
	hardwareSpeedSaveEnabled = false

	// hardwareSpeedReadEnabled turns on the stage 1 read-only readout at startup. Off: on firmware
	// 1.7.142 the CORE answers the documented speed-mode endpoint (60 6d) with data type 87 ab
	// instead of 03 00, on both channel 0x00 and 0x01 (tested 2026-11-08), so the layout liquidctl
	// documents doesn't apply here. Re-enable once a USB capture of iCUE shows the real format.
	hardwareSpeedReadEnabled = false

	// hwConfigChannel is the endpoint channel used to open, read, write and close the hardware
	// config endpoints. liquidctl uses channel 0x00 for them; OpenLinkHub uses 0x01 for its own
	// data reads. Which one firmware 1.7.142 expects for these endpoints is unverified. NOTE:
	// OpenLinkHub keeps its lighting endpoint (0x22) open on channel 0x00, so every use of this
	// channel closes the lighting endpoint, and callers must reopen it (setColorEndpoint).
	hwConfigChannel = byte(0x01)

	hwFanModeFixed     = byte(0x00)
	hwPumpMinimumDuty  = 75 // iCUE's lowest (Quiet) pump preset, per the liquidctl guide
	hwMaxCurvePoints   = 7
	hwMaxChannels      = 16
	hwReplyHeaderSize  = 5 // [report][cmd echo][status][data type 2 bytes]
	hwMoreReplyHeader  = 3 // continuation replies: [report][cmd echo][status]
	hwBaselineSubdir   = "/database/hardware-baseline/"
	hwWriteHeaderBytes = 4 // [len u16 LE incl. data type][00][00]
)

var (
	hwSpeedModeEndpoint    = []byte{0x60, 0x6d}
	hwFixedPercentEndpoint = []byte{0x61, 0x6d}
	hwCurveEndpoint        = []byte{0x62, 0x6d}
	hwSpeedModeType        = []byte{0x03, 0x00}
	hwFixedPercentType     = []byte{0x04, 0x00}
	hwCurveType            = []byte{0x05, 0x00}
)

// HardwareCurve is one channel's stored fan curve.
type HardwareCurve struct {
	Sensor byte     `json:"sensor"` // 0x00 liquid temperature, 0x01 external probe
	Points [][2]int `json:"points"` // [temperature in tenths of a degree C, percent]
}

// HardwareSpeedConfig is the parsed hardware speed configuration.
type HardwareSpeedConfig struct {
	Modes        []byte          `json:"modes"`
	FixedPercent []uint16        `json:"fixedPercent"`
	Curves       []HardwareCurve `json:"curves"`
	RawModes     string          `json:"rawModes"`
	RawFixed     string          `json:"rawFixed"`
	RawCurves    string          `json:"rawCurves"`
}

// hwReadEndpoint reads one hardware config endpoint the way liquidctl's _read_data does:
// open, read initial / more / final, close, then check the data type. The caller must hold
// deviceLock and must not be in the Exit state (transfer does not read replies then).
func (d *Device) hwReadEndpoint(endpoint, dataType []byte) ([]byte, string, error) {
	// Close first, as OpenLinkHub's own read() does, in case the channel was left open.
	_, _ = d.transfer([]byte{0x05, 0x01, hwConfigChannel}, nil, "hwRead")
	if _, err := d.transfer([]byte{0x0d, hwConfigChannel}, endpoint, "hwRead"); err != nil {
		return nil, "", err
	}

	var replies [3][]byte
	var readErr error
	for i := 0; i < 3; i++ {
		reply, err := d.transfer([]byte{0x08, hwConfigChannel, byte(i + 1)}, nil, "hwRead")
		if err != nil {
			readErr = err
			break
		}
		replies[i] = reply
	}

	// Always close, even after a failed read.
	_, closeErr := d.transfer([]byte{0x05, 0x01, hwConfigChannel}, nil, "hwRead")
	if readErr != nil {
		return nil, "", readErr
	}
	if closeErr != nil {
		return nil, "", closeErr
	}

	first := replies[0]
	raw := fmt.Sprintf("%x", first)
	if len(first) < hwReplyHeaderSize+1 {
		return nil, raw, fmt.Errorf("reply too short (%d bytes)", len(first))
	}
	if !bytes.Equal(first[3:5], dataType) {
		return nil, raw, fmt.Errorf("unexpected data type %x, want %x", first[3:5], dataType)
	}

	data := append([]byte{}, first[hwReplyHeaderSize:]...)
	for _, more := range replies[1:] {
		if len(more) > hwMoreReplyHeader {
			data = append(data, more[hwMoreReplyHeader:]...)
		}
	}
	return data, raw, nil
}

// hwWriteEndpoint writes a full table the way liquidctl's _write_data does: a read first to
// confirm the data type, then open, write [len][00 00][type][data], close. Tables written
// here are small enough for one packet. The caller must hold deviceLock.
func (d *Device) hwWriteEndpoint(endpoint, dataType, data []byte) error {
	if !hardwareSpeedSaveEnabled {
		return errors.New("hardware speed save is disabled")
	}
	if _, _, err := d.hwReadEndpoint(endpoint, dataType); err != nil {
		return fmt.Errorf("pre-write check failed: %w", err)
	}

	buffer := make([]byte, hwWriteHeaderBytes+len(dataType)+len(data))
	binary.LittleEndian.PutUint16(buffer[0:2], uint16(len(data)+len(dataType)))
	copy(buffer[hwWriteHeaderBytes:], dataType)
	copy(buffer[hwWriteHeaderBytes+len(dataType):], data)
	if len(buffer)+4 > bufferSizeWrite {
		return fmt.Errorf("table too large for one packet (%d bytes)", len(buffer))
	}

	if _, err := d.transfer([]byte{0x0d, hwConfigChannel}, endpoint, "hwWrite"); err != nil {
		return err
	}
	_, writeErr := d.transfer([]byte{0x06, hwConfigChannel}, buffer, "hwWrite")
	_, closeErr := d.transfer([]byte{0x05, 0x01, hwConfigChannel}, nil, "hwWrite")
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// parseHwModes parses [N][mode x N].
func parseHwModes(data []byte) ([]byte, error) {
	if len(data) < 1 {
		return nil, errors.New("empty speed mode table")
	}
	n := int(data[0])
	if n < 1 || n > hwMaxChannels || len(data) < 1+n {
		return nil, fmt.Errorf("implausible speed mode count %d", n)
	}
	return append([]byte{}, data[1:1+n]...), nil
}

// parseHwFixed parses [N][u16 LE x N].
func parseHwFixed(data []byte) ([]uint16, error) {
	if len(data) < 1 {
		return nil, errors.New("empty fixed percent table")
	}
	n := int(data[0])
	if n < 1 || n > hwMaxChannels || len(data) < 1+2*n {
		return nil, fmt.Errorf("implausible fixed percent count %d", n)
	}
	values := make([]uint16, n)
	for i := 0; i < n; i++ {
		values[i] = binary.LittleEndian.Uint16(data[1+2*i:])
		if values[i] > 100 {
			return nil, fmt.Errorf("channel %d fixed percent %d is above 100", i, values[i])
		}
	}
	return values, nil
}

// parseHwCurves parses [N] then per channel [sensor][points][points x (u16 temp*10, u16 %)],
// as liquidctl's set_speed_profile splits it.
func parseHwCurves(data []byte) ([]HardwareCurve, error) {
	if len(data) < 1 {
		return nil, errors.New("empty curve table")
	}
	n := int(data[0])
	if n < 1 || n > hwMaxChannels {
		return nil, fmt.Errorf("implausible curve count %d", n)
	}
	curves := make([]HardwareCurve, 0, n)
	i := 1
	for c := 0; c < n; c++ {
		if i+2 > len(data) {
			return nil, fmt.Errorf("curve %d header out of range", c)
		}
		sensor, points := data[i], int(data[i+1])
		if points > hwMaxCurvePoints {
			return nil, fmt.Errorf("curve %d has %d points", c, points)
		}
		end := i + 2 + 4*points
		if end > len(data) {
			return nil, fmt.Errorf("curve %d points out of range", c)
		}
		curve := HardwareCurve{Sensor: sensor}
		for p := 0; p < points; p++ {
			off := i + 2 + 4*p
			curve.Points = append(curve.Points, [2]int{
				int(binary.LittleEndian.Uint16(data[off:])),
				int(binary.LittleEndian.Uint16(data[off+2:])),
			})
		}
		curves = append(curves, curve)
		i = end
	}
	return curves, nil
}

// readHardwareSpeedConfig reads and validates all three hardware speed tables. The caller
// must hold deviceLock. It never writes.
func (d *Device) readHardwareSpeedConfig() (*HardwareSpeedConfig, error) {
	cfg := &HardwareSpeedConfig{}

	modes, raw, err := d.hwReadEndpoint(hwSpeedModeEndpoint, hwSpeedModeType)
	cfg.RawModes = raw
	if err != nil {
		return cfg, fmt.Errorf("speed mode table: %w", err)
	}
	if cfg.Modes, err = parseHwModes(modes); err != nil {
		return cfg, fmt.Errorf("speed mode table: %w", err)
	}

	fixed, raw, err := d.hwReadEndpoint(hwFixedPercentEndpoint, hwFixedPercentType)
	cfg.RawFixed = raw
	if err != nil {
		return cfg, fmt.Errorf("fixed percent table: %w", err)
	}
	if cfg.FixedPercent, err = parseHwFixed(fixed); err != nil {
		return cfg, fmt.Errorf("fixed percent table: %w", err)
	}
	if len(cfg.FixedPercent) != len(cfg.Modes) {
		return cfg, fmt.Errorf("channel count mismatch: %d modes, %d fixed values", len(cfg.Modes), len(cfg.FixedPercent))
	}

	curves, raw, err := d.hwReadEndpoint(hwCurveEndpoint, hwCurveType)
	cfg.RawCurves = raw
	if err != nil {
		return cfg, fmt.Errorf("curve table: %w", err)
	}
	if cfg.Curves, err = parseHwCurves(curves); err != nil {
		return cfg, fmt.Errorf("curve table: %w", err)
	}
	return cfg, nil
}

// logHardwareSpeedConfig is stage 1: read the hardware speed tables once at init, log them,
// and save a one-time baseline. It runs before the lighting endpoint is opened, so using
// channel 0x00 cannot disturb lighting. Errors are logged and never stop init.
func (d *Device) logHardwareSpeedConfig() {
	if !hardwareSpeedReadEnabled {
		return
	}
	d.deviceLock.Lock()
	cfg, err := d.readHardwareSpeedConfig()
	d.deviceLock.Unlock()

	if err != nil {
		logger.Log(logger.Fields{
			"serial": d.Serial, "error": err, "channel": hwConfigChannel,
			"rawModes": cfg.RawModes, "rawFixed": cfg.RawFixed, "rawCurves": cfg.RawCurves,
		}).Warn("Unable to read Commander CORE hardware speed config")
		return
	}

	logger.Log(logger.Fields{
		"serial": d.Serial, "channel": hwConfigChannel,
		"modes": fmt.Sprintf("%x", cfg.Modes), "fixedPercent": cfg.FixedPercent, "curves": cfg.Curves,
	}).Info("Commander CORE hardware speed config")

	dir := pwd + hwBaselineSubdir
	path := dir + d.Serial + ".json"
	if common.FileExists(path) {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		logger.Log(logger.Fields{"error": err, "location": dir}).Warn("Unable to create hardware baseline folder")
		return
	}
	if err := common.SaveJsonData(path, cfg); err != nil {
		logger.Log(logger.Fields{"error": err, "location": path}).Warn("Unable to save hardware baseline")
		return
	}
	logger.Log(logger.Fields{"serial": d.Serial, "location": path}).Info("Saved Commander CORE hardware speed baseline")
}

// recordSpeeds remembers the last percent duty OpenLinkHub applied per channel.
func (d *Device) recordSpeeds(data map[int]byte) {
	d.speedLock.Lock()
	defer d.speedLock.Unlock()
	if d.lastSpeed == nil {
		d.lastSpeed = make(map[int]byte, len(data))
	}
	for channel, value := range data {
		d.lastSpeed[channel] = value
	}
}

// saveHardwareSpeeds is stage 2: store the speeds OpenLinkHub is running as fixed hardware
// percentages. Only channels with a known current duty are changed; the pump never goes
// below hwPumpMinimumDuty. Both tables are read back and compared. The caller must hold
// deviceLock and must not be in the Exit state. Returns the number of channels stored.
func (d *Device) saveHardwareSpeeds() (int, error) {
	if !hardwareSpeedSaveEnabled {
		return 0, errors.New("hardware speed save is disabled")
	}

	d.speedLock.Lock()
	duties := make(map[int]byte, len(d.lastSpeed))
	for channel, value := range d.lastSpeed {
		duties[channel] = value
	}
	d.speedLock.Unlock()

	cfg, err := d.readHardwareSpeedConfig()
	if err != nil {
		return 0, err
	}

	modes := append([]byte{}, cfg.Modes...)
	fixed := append([]uint16{}, cfg.FixedPercent...)
	changed := 0
	for channel := range modes {
		duty, ok := duties[channel]
		if !ok {
			continue // unknown current duty: leave this channel's stored values alone
		}
		value := uint16(duty)
		if value > 100 {
			value = 100
		}
		if channel == 0 && value < hwPumpMinimumDuty { // channel 0 is the pump
			value = hwPumpMinimumDuty
		}
		modes[channel] = hwFanModeFixed
		fixed[channel] = value
		changed++
	}
	if changed == 0 {
		return 0, nil
	}

	modeTable := append([]byte{byte(len(modes))}, modes...)
	fixedTable := make([]byte, 1+2*len(fixed))
	fixedTable[0] = byte(len(fixed))
	for i, value := range fixed {
		binary.LittleEndian.PutUint16(fixedTable[1+2*i:], value)
	}

	if err := d.hwWriteEndpoint(hwSpeedModeEndpoint, hwSpeedModeType, modeTable); err != nil {
		return 0, fmt.Errorf("writing speed mode table: %w", err)
	}
	if err := d.hwWriteEndpoint(hwFixedPercentEndpoint, hwFixedPercentType, fixedTable); err != nil {
		return 0, fmt.Errorf("writing fixed percent table: %w", err)
	}

	check, err := d.readHardwareSpeedConfig()
	if err != nil {
		return 0, fmt.Errorf("read-back: %w", err)
	}
	if !bytes.Equal(check.Modes, modes) {
		return 0, fmt.Errorf("read-back mismatch: modes %x, wrote %x", check.Modes, modes)
	}
	for i := range fixed {
		if check.FixedPercent[i] != fixed[i] {
			return 0, fmt.Errorf("read-back mismatch: fixed percent %v, wrote %v", check.FixedPercent, fixed)
		}
	}

	logger.Log(logger.Fields{"serial": d.Serial, "channels": changed, "modes": fmt.Sprintf("%x", modes), "fixedPercent": fixed}).Info("Stored fan and pump speeds in Commander CORE hardware config")
	return changed, nil
}

// SaveHardwareLighting is called by POST /api/hardware/save (the "Controller" Save button).
// On the Commander CORE it stores fan and pump speeds only; lighting can't be stored on this
// device with any documented command. Returns 1 saved, 2 nothing to save, 0 disabled or failed.
func (d *Device) SaveHardwareLighting() uint8 {
	if !hardwareSpeedSaveEnabled {
		logger.Log(logger.Fields{"serial": d.Serial}).Warn("hardware speed save is disabled")
		return 0
	}
	if d.Exit {
		return 0
	}

	d.deviceLock.Lock()
	saved, err := d.saveHardwareSpeeds()
	d.deviceLock.Unlock()

	// The hardware config channel shares channel 0x00 with lighting, so reopen it.
	d.setColorEndpoint()

	if err != nil {
		logger.Log(logger.Fields{"serial": d.Serial, "error": err}).Error("Unable to store speeds in Commander CORE hardware config")
		return 0
	}
	if saved == 0 {
		return 2
	}
	return 1
}

// saveHardwareSpeedsOnStop runs at the start of Stop, before Exit is set, so the device still
// answers reads. Lighting is ending anyway, so the lighting endpoint is not reopened.
func (d *Device) saveHardwareSpeedsOnStop() {
	if !hardwareSpeedSaveEnabled {
		return
	}
	d.deviceLock.Lock()
	saved, err := d.saveHardwareSpeeds()
	d.deviceLock.Unlock()
	if err != nil {
		logger.Log(logger.Fields{"serial": d.Serial, "error": err}).Error("Unable to store speeds in Commander CORE hardware config on stop")
		return
	}
	logger.Log(logger.Fields{"serial": d.Serial, "channels": saved}).Info("Stored speeds in Commander CORE hardware config on stop")
}
