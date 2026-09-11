// Package inventory discovers physical Linux host facts without configuring hardware.
package inventory

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
)

// Source allows deterministic discovery tests without depending on the development VM.
// Files paths are relative to the Linux root. Hostname is human-facing only.
type Source struct {
	Files        fs.FS
	Hostname     func() (string, error)
	Architecture string
}

func Local() Source {
	return Source{Files: os.DirFS("/"), Hostname: os.Hostname, Architecture: runtime.GOARCH}
}

// Discover returns partial inventory with explicit warnings when facts are unavailable.
// Zero topology/memory counts mean unknown; missing GPU discovery does not imply no GPUs.
func (s Source) Discover(hostID identity.HostID) (model.Inventory, []string) {
	result := model.Inventory{SchemaVersion: protocol.CurrentSchemaVersion,
		Host: model.Host{HostID: hostID, Architecture: s.Architecture}, GPUs: []model.GPU{}}
	warnings := []string{}
	warn := func(name string, err error) { warnings = append(warnings, name+": "+err.Error()) }
	hostname, err := s.Hostname()
	if err != nil {
		warn("hostname", err)
	} else {
		result.Host.Hostname = hostname
	}
	release, err := fs.ReadFile(s.Files, "etc/os-release")
	if errors.Is(err, fs.ErrNotExist) {
		release, err = fs.ReadFile(s.Files, "usr/lib/os-release")
	}
	if err != nil {
		warn("os-release", err)
	} else {
		values, err := parseOSRelease(string(release))
		if err != nil {
			warn("os-release", err)
		}
		result.Host.OS.ID = values["ID"]
		result.Host.OS.VersionID = values["VERSION_ID"]
		result.Host.OS.PrettyName = values["PRETTY_NAME"]
	}
	kernel, err := fs.ReadFile(s.Files, "proc/sys/kernel/osrelease")
	if err != nil {
		warn("kernel", err)
	} else {
		result.Host.OS.Kernel = strings.TrimSpace(string(kernel))
	}
	cpu, err := fs.ReadFile(s.Files, "proc/cpuinfo")
	if err != nil {
		warn("CPU", err)
	} else {
		result.CPU, err = parseCPU(string(cpu))
		if err != nil {
			warn("CPU", err)
		}
	}
	memory, err := fs.ReadFile(s.Files, "proc/meminfo")
	if err != nil {
		warn("memory", err)
	} else {
		result.Memory.TotalBytes, err = parseMemory(string(memory))
		if err != nil {
			warn("memory", err)
		}
	}
	gpus, err := discoverGPUs(s.Files)
	if err != nil {
		warn("GPU", err)
	} else {
		result.GPUs = gpus
	}
	return result, warnings
}

func discoverGPUs(files fs.FS) ([]model.GPU, error) {
	entries, err := fs.ReadDir(files, "sys/bus/pci/devices")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []model.GPU
	seen := make(map[string]struct{})
	for _, entry := range entries {
		info, statErr := fs.Stat(files, "sys/bus/pci/devices/"+entry.Name())
		if statErr != nil || !info.IsDir() {
			continue
		}
		pci := strings.ToLower(strings.TrimSpace(entry.Name()))
		base := "sys/bus/pci/devices/" + entry.Name() + "/"
		class, err := readTrimmed(files, base+"class")
		if err != nil || !strings.HasPrefix(strings.ToLower(class), "0x03") {
			continue
		}
		vendorID, _ := readTrimmed(files, base+"vendor")
		modelID, _ := readTrimmed(files, base+"device")
		uuid := ""
		for _, name := range []string{"unique_id", "gpu_uuid"} {
			if value, readErr := readTrimmed(files, base+name); readErr == nil && value != "" {
				uuid = value
				break
			}
		}
		if uuid == "" {
			if info, readErr := fs.ReadFile(files, "proc/driver/nvidia/gpus/"+entry.Name()+"/information"); readErr == nil {
				uuid = parseNVIDIAUUID(string(info))
			}
		}
		uuid = normalizeStableIdentity(uuid)
		if !validStableIdentity(uuid) {
			return nil, fmt.Errorf("GPU at %s has no stable UUID/unique identity", pci)
		}
		if _, duplicate := seen[uuid]; duplicate {
			return nil, errors.New("GPU inventory contains duplicate stable identities")
		}
		seen[uuid] = struct{}{}
		deviceID, err := deviceIDFromStableIdentity(uuid)
		if err != nil {
			return nil, err
		}
		result = append(result, model.GPU{DeviceID: deviceID, Vendor: gpuVendor(vendorID), Model: strings.ToLower(strings.TrimSpace(modelID)), PCIBusID: pci, UUID: uuid})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DeviceID.String() < result[j].DeviceID.String() })
	return result, nil
}

func readTrimmed(files fs.FS, name string) (string, error) {
	value, err := fs.ReadFile(files, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}

func parseNVIDIAUUID(text string) string {
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "GPU UUID") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeStableIdentity(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func validStableIdentity(value string) bool {
	switch value {
	case "", "unknown", "none", "n/a", "not available":
		return false
	}
	return strings.Trim(value, "0-:._ ") != ""
}

func deviceIDFromStableIdentity(value string) (identity.DeviceID, error) {
	sum := sha256.Sum256([]byte("gpu-identity-v1\x00" + value))
	return identity.ParseDeviceID(fmt.Sprintf("device_%x", sum[:16]))
}

func gpuVendor(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0x10de":
		return "nvidia"
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

// os-release is data, never executed or expanded as shell input.
func parseOSRelease(text string) (map[string]string, error) {
	values := make(map[string]string)
	var problems []error
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			problems = append(problems, errors.New("malformed os-release line"))
			continue
		}
		key = strings.TrimSpace(key)
		// Only fields used by this inventory are interpreted.
		if key != "ID" && key != "VERSION_ID" && key != "PRETTY_NAME" {
			continue
		}
		value, err := releaseValue(strings.TrimSpace(raw))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", key, err))
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, err)
	}
	return values, errors.Join(problems...)
}

func releaseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	quote := byte(0)
	if raw[0] == '\'' || raw[0] == '"' {
		quote = raw[0]
		if len(raw) < 2 || raw[len(raw)-1] != quote {
			return "", errors.New("unterminated quoted value")
		}
		raw = raw[1 : len(raw)-1]
	}
	if quote == '\'' {
		return raw, nil
	}
	var value strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			if i+1 == len(raw) {
				return "", errors.New("trailing escape")
			}
			next := raw[i+1]
			if quote == 0 || strings.ContainsRune("\"\\$`", rune(next)) {
				value.WriteByte(next)
				i++
				continue
			}
		}
		if raw[i] == '"' || (quote == 0 && (raw[i] == '\'' || raw[i] == ' ' || raw[i] == '\t')) {
			return "", errors.New("invalid quoting")
		}
		value.WriteByte(raw[i])
	}
	return value.String(), nil
}

func parseCPU(text string) (model.CPU, error) {
	var cpu model.CPU
	records := []map[string]string{}
	record := make(map[string]string)
	flush := func() {
		if _, ok := record["processor"]; ok {
			records = append(records, record)
		}
		record = make(map[string]string)
	}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if ok {
			record[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return cpu, err
	}
	if len(records) == 0 {
		return cpu, errors.New("no logical processors reported")
	}
	sockets := make(map[string]bool)
	cores := make(map[[2]string]bool)
	threads := make(map[string]bool)
	completeSockets, completeCores := true, true
	for _, r := range records {
		processor := r["processor"]
		if _, err := strconv.ParseUint(processor, 10, 32); err != nil {
			return cpu, errors.New("invalid logical processor ID")
		}
		if threads[processor] {
			return cpu, errors.New("duplicate logical processor ID")
		}
		threads[processor] = true
		if cpu.Vendor == "" {
			cpu.Vendor = r["vendor_id"]
		}
		if cpu.Model == "" {
			cpu.Model = r["model name"]
		}
		socket, core := r["physical id"], r["core id"]
		if _, err := strconv.ParseUint(socket, 10, 32); err != nil {
			completeSockets = false
		}
		if _, err := strconv.ParseUint(core, 10, 32); err != nil {
			completeCores = false
		}
		sockets[socket] = true
		cores[[2]string{socket, core}] = true
	}
	cpu.Threads = uint32(len(threads))
	if completeSockets {
		cpu.Sockets = uint32(len(sockets))
	}
	if completeSockets && completeCores {
		cpu.Cores = uint32(len(cores))
	}
	if !completeSockets || !completeCores {
		return cpu, errors.New("physical topology unavailable; unknown socket/core counts are zero")
	}
	return cpu, nil
}

func parseMemory(text string) (uint64, error) {
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, errors.New("invalid MemTotal unit or format")
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || kib == 0 || kib > math.MaxUint64/1024 {
			return 0, errors.New("invalid MemTotal value")
		}
		return kib * 1024, nil
	}
	return 0, errors.New("MemTotal missing")
}
