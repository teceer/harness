package sysstat

import "testing"

func TestParseVMStat(t *testing.T) {
	out := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                5725.
Pages active:                            100.
Pages inactive:                          602160.
Pages wired down:                        50.
Pages occupied by compressor:            10.
`
	if got := parseVMStat(out); got != 160*16384 {
		t.Fatalf("parseVMStat = %v", got)
	}
}

func TestParseSwap(t *testing.T) {
	if got := parseSwap("total = 2048.00M  used = 1022.25M  free = 1025.75M  (encrypted)"); got != 1022.25*(1<<20) {
		t.Fatalf("parseSwap = %v", got)
	}
	if parseSwap("garbage") != 0 {
		t.Fatal("garbage should give 0")
	}
}

func TestParseIostat(t *testing.T) {
	out := "              disk0 \n    KB/t xfrs   MB \n   18.63 794056885 14444251.80 \n"
	if got := parseIostat(out); got != 14444251.80 {
		t.Fatalf("parseIostat = %v", got)
	}
}

func TestParseNetstatSumsLinkRowsOfEnOnly(t *testing.T) {
	out := `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                         10     0       999      10     0       999     0
en0        1500  <Link#14>   06:3c:1a:d4:55:0d 141967329     0 1000 97654561     0 300     0
en0        1500  macbook-pro fe80:e::9b:e912:5 141967329     - 1000 97654561     - 300     -
en1        1500  <Link#15>   06:3c:1a:d4:55:0e 1     0 20 1     0 5     0
utun3      1380  <Link#20>                        1     0 7777 1     0 7777     0
`
	in, out2 := parseNetstat(out)
	if in != 1020 || out2 != 305 {
		t.Fatalf("parseNetstat = %d, %d", in, out2)
	}
}

func TestRateIgnoresCounterReset(t *testing.T) {
	if rate(10, 20, 5) != 0 || rate(30, 20, 5) != 2 {
		t.Fatal("rate")
	}
}

func TestSamplerProducesLiveReadings(t *testing.T) {
	var s Sampler
	s.take()
	s.take()
	h := s.hist
	if len(h) != 1 {
		t.Fatalf("want 1 sample, got %d", len(h))
	}
	if h[0].MemTotal <= 0 || h[0].DiskSize <= 0 || h[0].MemUsed <= 0 {
		t.Fatalf("empty reading: %+v", h[0])
	}
	t.Logf("%+v", h[0])
}

func TestPolishDecimalComma(t *testing.T) {
	if got := parseLoad("{ 12,83 16,27 17,80 }"); got != 12.83 {
		t.Fatalf("parseLoad = %v", got)
	}
	if got := parseSwap("total = 15360,00M  used = 14426,56M  free = 933,44M"); got != 14426.56*(1<<20) {
		t.Fatalf("parseSwap = %v", got)
	}
}
