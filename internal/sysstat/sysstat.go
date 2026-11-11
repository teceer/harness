// Package sysstat samples the machine the agents run on (CPU, memory,
// temperature, disk and network) into a short rolling history for the board.
package sysstat

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Sample is one reading. Rates are per second since the previous sample.
type Sample struct {
	At       int64   `json:"at"`       // unix ms
	CPU      float64 `json:"cpu"`      // % of all cores
	Load     float64 `json:"load"`     // 1-minute load average
	MemUsed  float64 `json:"memUsed"`  // bytes in use (active + wired + compressed)
	MemTotal float64 `json:"memTotal"` // bytes
	Swap     float64 `json:"swap"`     // bytes of swap in use
	TempCPU  float64 `json:"tempCpu"`  // °C, mean of the CPU die sensors; 0 when unknown
	TempMax  float64 `json:"tempMax"`  // °C, hottest sensor
	DiskUsed float64 `json:"diskUsed"` // bytes used on the data volume
	DiskSize float64 `json:"diskSize"` // bytes
	DiskIO   float64 `json:"diskIO"`   // bytes/s read + written on disk0
	NetIn    float64 `json:"netIn"`    // bytes/s received on en* interfaces
	NetOut   float64 `json:"netOut"`   // bytes/s sent
}

const (
	Every = 5 * time.Second
	Keep  = 360 // 30 minutes at Every
)

// Sampler keeps the history; it starts on first use and stops sampling
// when nobody has asked for a while, so an idle server costs nothing.
type Sampler struct {
	mu      sync.Mutex
	hist    []Sample
	asked   time.Time
	running bool
	prev    counters
}

type counters struct {
	at          time.Time
	cpuBusy     uint64
	cpuTotal    uint64
	diskMB      float64
	netIn       uint64
	netOut      uint64
	initialized bool
}

// History returns the samples so far and keeps the sampler alive.
func (s *Sampler) History() []Sample {
	s.mu.Lock()
	s.asked = time.Now()
	if !s.running {
		s.running = true
		go s.loop()
	}
	out := append([]Sample(nil), s.hist...)
	s.mu.Unlock()
	return out
}

func (s *Sampler) loop() {
	s.take() // prime the counters so the first rates are real
	t := time.NewTicker(Every)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		idle := time.Since(s.asked) > 2*time.Minute
		if idle {
			s.running = false
		}
		s.mu.Unlock()
		if idle {
			return
		}
		s.take()
	}
}

func (s *Sampler) take() {
	now := time.Now()
	cur := counters{at: now, initialized: true}
	cur.cpuBusy, cur.cpuTotal = cpuTicks()
	cur.diskMB = diskMB()
	cur.netIn, cur.netOut = netBytes()
	smp := Sample{At: now.UnixMilli(), Load: loadAvg(), Swap: swapUsed()}
	smp.MemUsed, smp.MemTotal = memory()
	smp.TempCPU, smp.TempMax = temperatures()
	smp.DiskUsed, smp.DiskSize = diskSpace("/System/Volumes/Data")

	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.prev
	s.prev = cur
	if !p.initialized {
		return
	}
	dt := now.Sub(p.at).Seconds()
	if dt <= 0 {
		return
	}
	if db := cur.cpuTotal - p.cpuTotal; cur.cpuTotal > p.cpuTotal && db > 0 {
		smp.CPU = 100 * float64(cur.cpuBusy-p.cpuBusy) / float64(db)
	}
	smp.DiskIO = rate(cur.diskMB*1024*1024, p.diskMB*1024*1024, dt)
	smp.NetIn = rate(float64(cur.netIn), float64(p.netIn), dt)
	smp.NetOut = rate(float64(cur.netOut), float64(p.netOut), dt)
	s.hist = append(s.hist, smp)
	if len(s.hist) > Keep {
		s.hist = s.hist[len(s.hist)-Keep:]
	}
}

// rate is the per-second change of a counter; a counter that went back
// (reboot, interface reset) gives 0, not a negative spike.
func rate(cur, prev, dt float64) float64 {
	if cur < prev {
		return 0
	}
	return (cur - prev) / dt
}

func run(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // a Polish locale prints "16,27"
	out, _ := cmd.Output()
	return string(out)
}

// num parses a decimal written either way, in case a tool ignores LC_ALL.
func num(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	return v
}

func loadAvg() float64 {
	// "{ 3.21 2.98 2.75 }"
	return parseLoad(run("sysctl", "-n", "vm.loadavg"))
}

func parseLoad(out string) float64 {
	f := strings.Fields(strings.Trim(out, "{} \n"))
	if len(f) == 0 {
		return 0
	}
	return num(f[0])
}

func memory() (used, total float64) {
	t, _ := strconv.ParseFloat(strings.TrimSpace(run("sysctl", "-n", "hw.memsize")), 64)
	return parseVMStat(run("vm_stat")), t
}

// parseVMStat counts what Activity Monitor calls memory used: app (active),
// wired and compressed pages.
func parseVMStat(out string) float64 {
	page := 4096.0
	var pages float64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "page size of "); i >= 0 {
			if v, err := strconv.ParseFloat(strings.Fields(line[i+len("page size of "):])[0], 64); err == nil {
				page = v
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages active", "Pages wired down", "Pages occupied by compressor":
			n, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "."), 64)
			pages += n
		}
	}
	return pages * page
}

func swapUsed() float64 {
	// "total = 2048.00M  used = 1022.25M  free = 1025.75M  (encrypted)"
	return parseSwap(run("sysctl", "-n", "vm.swapusage"))
}

func parseSwap(out string) float64 {
	_, rest, ok := strings.Cut(out, "used = ")
	if !ok {
		return 0
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return 0
	}
	return parseSize(f[0])
}

func parseSize(s string) float64 {
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult = 1 << 10
	case strings.HasSuffix(s, "M"):
		mult = 1 << 20
	case strings.HasSuffix(s, "G"):
		mult = 1 << 30
	}
	return num(strings.TrimRight(s, "KMG")) * mult
}

func diskSpace(path string) (used, size float64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0
	}
	bs := float64(st.Bsize)
	size = float64(st.Blocks) * bs
	// Used as the user sees it: what is not available to them.
	return size - float64(st.Bavail)*bs, size
}

// diskMB is the total MB transferred on disk0 since boot.
func diskMB() float64 { return parseIostat(run("iostat", "-Id", "disk0")) }

func parseIostat(out string) float64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 3 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[2], 64)
	return v
}

func netBytes() (in, out uint64) { return parseNetstat(run("netstat", "-ib")) }

// parseNetstat sums the link-level counters of the physical en* interfaces;
// tunnels (utun) would count the same traffic twice.
func parseNetstat(out string) (in, outB uint64) {
	sc := bufio.NewScanner(strings.NewReader(out))
	head := map[string]int{}
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(head) == 0 {
			for i, h := range f {
				head[h] = i
			}
			continue
		}
		if len(f) < len(head) || !strings.HasPrefix(f[0], "en") || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		ib, _ := strconv.ParseUint(f[head["Ibytes"]], 10, 64)
		ob, _ := strconv.ParseUint(f[head["Obytes"]], 10, 64)
		in += ib
		outB += ob
	}
	return in, outB
}
