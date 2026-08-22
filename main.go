package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	defaultInterval      = 2 * time.Second
	defaultPortSpeedGbps = 1.0
)

func init() {
	_ = godotenv.Load()
}

// Overridable via env vars (e.g. an EnvironmentFile= for systemd, or
// --env-file for Docker) rather than requiring a recompile per box:
//
//	NETWORK_IF        interface to monitor — skip auto-detect entirely
//	PORT_SPEED_GBPS   used only if the NIC doesn't report its own link
//	                   speed (common on virtualized/cloud NICs)
//	INTERVAL_SECONDS  refresh interval
func loadConfig() (iface string, portSpeedGbps float64, interval time.Duration) {
	iface = os.Getenv("NETWORK_IF")

	portSpeedGbps = defaultPortSpeedGbps
	if v := os.Getenv("PORT_SPEED_GBPS"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			portSpeedGbps = parsed
		}
	}

	interval = defaultInterval
	if v := os.Getenv("INTERVAL_SECONDS"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			interval = time.Duration(parsed * float64(time.Second))
		}
	}

	return
}

type NetStats struct {
	RX uint64
	TX uint64
}

type DiskStats struct {
	ReadBytes  uint64
	WriteBytes uint64
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func cpuStats() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)

	if scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && fields[0] == "cpu" {
			for i := 1; i < len(fields); i++ {
				v, _ := strconv.ParseUint(fields[i], 10, 64)
				total += v

				// idle + iowait
				if i == 4 || i == 5 {
					idle += v
				}
			}
		}
	}

	return
}

func cpuUsage(prevIdle, prevTotal, idle, total uint64) float64 {
	idleDelta := idle - prevIdle
	totalDelta := total - prevTotal

	if totalDelta == 0 {
		return 0
	}

	return 100 * (1 - float64(idleDelta)/float64(totalDelta))
}

func memoryStats() (used, total uint64) {
	data := readFile("/proc/meminfo")

	var memTotal, memAvailable uint64

	scanner := bufio.NewScanner(strings.NewReader(data))

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) < 2 {
			continue
		}

		value, _ := strconv.ParseUint(fields[1], 10, 64)

		switch fields[0] {
		case "MemTotal:":
			memTotal = value * 1024

		case "MemAvailable:":
			memAvailable = value * 1024
		}
	}

	return memTotal - memAvailable, memTotal
}

// virtualIfacePrefixes are interfaces Docker/libvirt/k8s create locally —
// container/bridge traffic, not the box's actual internet-facing link. A
// LiveKit host is normally also a Docker host (CLAUDE.md's docker-compose
// setup), so without filtering these out, auto-detect picks one of these
// over the real NIC just because it reads earlier from /sys/class/net.
var virtualIfacePrefixes = []string{"lo", "docker", "veth", "br-", "virbr", "tun", "tap", "cni", "flannel"}

func isVirtualIface(name string) bool {
	for _, prefix := range virtualIfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func detectInterface(configured string) string {
	if configured != "" {
		return configured
	}

	// The default-route interface is the one actually carrying
	// internet-bound traffic — the only reliable way to identify "the real
	// NIC" rather than guessing from interface naming conventions, which
	// vary across providers (eth0, ens3, enp1s0, ...).
	if iface := defaultRouteInterface(); iface != "" {
		return iface
	}

	// Fallback: first non-virtual interface /sys/class/net reports.
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if !isVirtualIface(entry.Name()) {
			return entry.Name()
		}
	}

	return ""
}

// defaultRouteInterface reads the kernel routing table for the interface
// with a default route (destination 00000000, i.e. 0.0.0.0/0).
func defaultRouteInterface() string {
	data := readFile("/proc/net/route")

	scanner := bufio.NewScanner(strings.NewReader(data))
	firstLine := true

	for scanner.Scan() {
		if firstLine {
			firstLine = false
			continue // header row: Iface Destination Gateway Flags ...
		}

		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		if fields[1] == "00000000" {
			return fields[0]
		}
	}

	return ""
}

// detectPortSpeedGbps reads the NIC's own negotiated link speed rather than
// trusting a manually configured guess. Falls back to `configured` when the
// driver doesn't expose it — common on virtualized/cloud NICs, which often
// report -1 here since there's no physical link to negotiate a speed on.
func detectPortSpeedGbps(iface string, configured float64) float64 {
	raw := strings.TrimSpace(readFile("/sys/class/net/" + iface + "/speed"))

	mbps, err := strconv.ParseFloat(raw, 64)
	if err != nil || mbps <= 0 {
		return configured
	}

	return mbps / 1000
}

func networkStats(iface string) NetStats {
	data := readFile("/proc/net/dev")

	scanner := bufio.NewScanner(strings.NewReader(data))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.Contains(line, ":") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)

		if strings.TrimSpace(parts[0]) != iface {
			continue
		}

		fields := strings.Fields(parts[1])

		if len(fields) < 9 {
			return NetStats{}
		}

		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)

		return NetStats{
			RX: rx,
			TX: tx,
		}
	}

	return NetStats{}
}

func diskStats() DiskStats {
	data := readFile("/proc/diskstats")

	var result DiskStats

	scanner := bufio.NewScanner(strings.NewReader(data))

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) < 14 {
			continue
		}

		// Ignore loop, ram and device-mapper devices.
		device := fields[2]

		if strings.HasPrefix(device, "loop") ||
			strings.HasPrefix(device, "ram") ||
			strings.HasPrefix(device, "dm-") {
			continue
		}

		reads, _ := strconv.ParseUint(fields[5], 10, 64)
		writes, _ := strconv.ParseUint(fields[9], 10, 64)

		// Linux diskstats sectors are normally 512 bytes.
		result.ReadBytes += reads * 512
		result.WriteBytes += writes * 512
	}

	return result
}

func diskUsage() (used, total uint64, percent float64) {
	cmd := exec.Command("df", "-B1", "/")

	output, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")

	if len(lines) < 2 {
		return
	}

	fields := strings.Fields(lines[len(lines)-1])

	if len(fields) < 5 {
		return
	}

	total, _ = strconv.ParseUint(fields[1], 10, 64)
	used, _ = strconv.ParseUint(fields[2], 10, 64)

	if total > 0 {
		percent = float64(used) / float64(total) * 100
	}

	return
}

func loadAverage() (float64, float64, float64) {
	data := readFile("/proc/loadavg")

	fields := strings.Fields(data)

	if len(fields) < 3 {
		return 0, 0, 0
	}

	a, _ := strconv.ParseFloat(fields[0], 64)
	b, _ := strconv.ParseFloat(fields[1], 64)
	c, _ := strconv.ParseFloat(fields[2], 64)

	return a, b, c
}

func formatBytes(bytes float64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", bytes/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", bytes/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", bytes/KB)
	default:
		return fmt.Sprintf("%.0f B", bytes)
	}
}

func formatMbps(bytesPerSecond float64) string {
	return fmt.Sprintf("%.2f Mbps", bytesPerSecond*8/1_000_000)
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func main() {
	configuredIF, configuredPortSpeed, interval := loadConfig()

	iface := detectInterface(configuredIF)
	if iface == "" {
		fmt.Println("Could not detect network interface.")
		fmt.Println("Set NETWORK_IF to the interface to monitor and try again.")
		os.Exit(1)
	}

	portSpeedGbps := detectPortSpeedGbps(iface, configuredPortSpeed)

	fmt.Println("LiveKit server monitor")
	fmt.Println("Network interface:", iface)
	fmt.Printf("Port speed:       %.2f Gbps\n", portSpeedGbps)
	fmt.Println("Starting...")

	prevIdle, prevTotal := cpuStats()
	prevNet := networkStats(iface)
	prevDisk := diskStats()

	time.Sleep(interval)

	for {
		idle, totalCPU := cpuStats()

		cpu := cpuUsage(
			prevIdle,
			prevTotal,
			idle,
			totalCPU,
		)

		prevIdle = idle
		prevTotal = totalCPU

		usedMem, totalMem := memoryStats()

		var memPercent float64
		if totalMem > 0 {
			memPercent = float64(usedMem) / float64(totalMem) * 100
		}

		net := networkStats(iface)

		rxBytes := float64(net.RX - prevNet.RX)
		txBytes := float64(net.TX - prevNet.TX)

		rxPerSec := rxBytes / interval.Seconds()
		txPerSec := txBytes / interval.Seconds()

		prevNet = net

		portMbps := portSpeedGbps * 1000

		rxMbps := rxPerSec * 8 / 1_000_000
		txMbps := txPerSec * 8 / 1_000_000

		rxUtil := rxMbps / portMbps * 100
		txUtil := txMbps / portMbps * 100

		disk := diskStats()

		diskRead := float64(disk.ReadBytes-prevDisk.ReadBytes) / interval.Seconds()
		diskWrite := float64(disk.WriteBytes-prevDisk.WriteBytes) / interval.Seconds()

		prevDisk = disk

		diskUsed, diskTotal, diskPercent := diskUsage()

		load1, load5, load15 := loadAverage()

		clearScreen()

		fmt.Println("======================================================")
		fmt.Println("                 LIVEKIT SERVER STATS")
		fmt.Println("======================================================")

		fmt.Printf("Time:             %s\n", time.Now().Format("2006-01-02 15:04:05"))
		fmt.Printf("Interface:        %s\n", iface)
		fmt.Println()

		fmt.Println("CPU")
		fmt.Println("------------------------------------------------------")
		fmt.Printf("Usage:            %.1f%%\n", cpu)
		fmt.Printf("Load:             %.2f / %.2f / %.2f\n", load1, load5, load15)
		fmt.Println()

		fmt.Println("MEMORY")
		fmt.Println("------------------------------------------------------")
		fmt.Printf("Used:             %s / %s\n",
			formatBytes(float64(usedMem)),
			formatBytes(float64(totalMem)),
		)
		fmt.Printf("Usage:            %.1f%%\n", memPercent)
		fmt.Println()

		fmt.Println("NETWORK")
		fmt.Println("------------------------------------------------------")
		fmt.Printf("Port speed:       %.1f Gbps\n", portSpeedGbps)
		fmt.Printf("IN:               %s\n", formatMbps(rxPerSec))
		fmt.Printf("OUT:              %s\n", formatMbps(txPerSec))
		fmt.Printf("IN utilization:   %.1f%%\n", rxUtil)
		fmt.Printf("OUT utilization:  %.1f%%\n", txUtil)
		fmt.Printf("Total RX:         %s\n", formatBytes(float64(net.RX)))
		fmt.Printf("Total TX:         %s\n", formatBytes(float64(net.TX)))
		fmt.Println()

		fmt.Println("DISK")
		fmt.Println("------------------------------------------------------")
		fmt.Printf("Usage:            %.1f%% (%s / %s)\n",
			diskPercent,
			formatBytes(float64(diskUsed)),
			formatBytes(float64(diskTotal)),
		)
		fmt.Printf("Read:             %s/s\n", formatBytes(diskRead))
		fmt.Printf("Write:            %s/s\n", formatBytes(diskWrite))
		fmt.Println()

		fmt.Println("SCALING SIGNAL")
		fmt.Println("------------------------------------------------------")

		warnings := 0

		if cpu >= 80 {
			fmt.Println("⚠ CPU is HIGH")
			warnings++
		}

		if memPercent >= 80 {
			fmt.Println("⚠ RAM is HIGH")
			warnings++
		}

		if txUtil >= 70 {
			fmt.Println("⚠ Network OUT is HIGH")
			warnings++
		}

		if diskPercent >= 85 {
			fmt.Println("⚠ Disk usage is HIGH")
			warnings++
		}

		if warnings == 0 {
			fmt.Println("✓ Server looks healthy")
		} else if warnings >= 2 {
			fmt.Println("⚠ Consider adding another LiveKit node")
		}

		fmt.Println()
		fmt.Printf("Refreshing every %s...\n", interval)
		fmt.Println("Press Ctrl+C to exit.")

		time.Sleep(interval)
	}
}
