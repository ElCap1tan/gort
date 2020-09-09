// Copyright (c) 2020 Yannic Wehner
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package pScan

import (
	"context"
	"fmt"
	"github.com/ElCap1tan/gort/internal/colorFmt"
	"github.com/ElCap1tan/gort/internal/helper"
	"github.com/ElCap1tan/gort/internal/helper/ulimit"
	"github.com/ElCap1tan/gort/internal/symbols"
	"github.com/ElCap1tan/gort/netUtil"
	"github.com/ElCap1tan/gort/netUtil/macLookup"
	"github.com/mdlayher/arp"
	quickArp "github.com/mostlygeek/arp"
	"github.com/sparrc/go-ping"
	"golang.org/x/sync/semaphore"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Targets []*Target

type HostName string

type TargetStatus int
type NetworkLocation int

const (
	Online TargetStatus = iota + 1
	OfflineFiltered
	Unknown
	Local NetworkLocation = iota + 1
	Global
	UnknownLoc
)

type Target struct {
	HostName      HostName
	Vendor        string
	IPAddr        net.IP
	MACAddr       net.HardwareAddr
	InitialTarget string
	Status        TargetStatus
	Location      NetworkLocation
	Ports         netUtil.Ports
	Rtts          []time.Duration
}

func NewTarget(t string, ports netUtil.Ports, privileged bool) *Target {
	h := &Target{InitialTarget: t, Ports: ports, Status: Unknown}
	h.Resolve()
	if h.IPAddr != nil {
		stats, _ := h.Ping(3, privileged)
		if stats.PacketsRecv > 0 {
			h.Status = Online
		}
		h.QueryMac()
		h.LookUpVendor()
	} else {
		h.MACAddr = nil
		h.Location = UnknownLoc
	}
	return h
}

func AsyncNewTarget(t string, ports netUtil.Ports, ch chan *Target, scanLock *semaphore.Weighted, privileged bool) {
	// TODO Add writeMutex
	scanLock.Acquire(context.TODO(), 4)
	h := &Target{InitialTarget: t, Ports: ports, Status: Unknown}
	h.Resolve()
	if h.IPAddr != nil {
		stats, _ := h.Ping(3, privileged)
		if stats.PacketsRecv > 0 {
			h.Status = Online
		}
		h.QueryMac()
		h.LookUpVendor()
	} else {
		h.MACAddr = nil
		h.Location = UnknownLoc
	}
	scanLock.Release(3)
	ch <- h
}

func ParseHostString(hostArgs string, ports netUtil.Ports, privileged bool) Targets {
	var tgtHosts Targets
	hostCount := 0
	out := make(chan *Target)

	var limit int64
	l, err := ulimit.GetUlimit()
	if err != nil {
		limit = 1024
	} else {
		limit = int64(l)
	}

	lock := semaphore.NewWeighted(limit)

	hosts := strings.Split(hostArgs, ",")
	for _, hostArg := range hosts {
		if ip, ipNet, err := net.ParseCIDR(hostArg); err == nil {
			for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); helper.IncIp(ip) {
				go AsyncNewTarget(ip.String(), ports, out, lock, privileged)
				hostCount++
			}
		} else if helper.ValidateIPOrRange(hostArg) {
			if strings.Contains(hostArg, "-") {
				addrParts := strings.Split(hostArg, ".")
				var octets [4][]int
				for i, addrPart := range addrParts {
					if strings.Contains(addrPart, "-") {
						octets[i] = append(octets[i], helper.StrRangeToArray(addrPart)...)
					} else {
						p, _ := strconv.Atoi(addrPart)
						octets[i] = append(octets[i], p)
					}
				}
				for _, t := range octetsToTargets(octets) {
					go AsyncNewTarget(t, ports, out, lock, privileged)
					hostCount++
				}
			} else {
				go AsyncNewTarget(hostArg, ports, out, lock, privileged)
				hostCount++
			}
		} else {
			go AsyncNewTarget(hostArg, ports, out, lock, privileged)
			hostCount++
		}
	}
	for i := 1; i <= hostCount; i++ {
		tgtHosts = append(tgtHosts, <-out)
	}
	return tgtHosts
}

func (t *Target) Resolve() {
	if helper.ValidateIPOrRange(t.InitialTarget) {
		hostNames, err := net.LookupAddr(t.InitialTarget)
		if err != nil {
			t.HostName = "N/A"
		} else {
			t.HostName = HostName(hostNames[0])
		}
		t.IPAddr = net.ParseIP(t.InitialTarget)
	} else {
		ips, err := net.LookupIP(t.InitialTarget)
		if err == nil {
			t.IPAddr = ips[0]

			hostNames, err := net.LookupAddr(string(t.IPAddr))
			if err == nil {
				t.HostName = HostName(hostNames[0])
			} else {
				t.HostName = HostName(t.InitialTarget)
			}
		} else {
			t.IPAddr = nil
			t.Status = OfflineFiltered
		}
	}
}

func (t *Target) QueryMac() {
	if b, err := t.IsHost(); b == true && err == nil {
		return
	}
	if macAddr, err := net.ParseMAC(quickArp.Search(t.IPAddr.String())); err == nil && macAddr.String() != "00:00:00:00:00:00" {
		colorFmt.Infof("%s Found MAC address for target '%s' via arp cache lookup: %s\n",
			symbols.INFO, t.InitialTarget, macAddr.String())
		t.Location = Local
		t.MACAddr = macAddr
		return
	}
	// Fallback if not found in arp cache
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Location = UnknownLoc
		t.MACAddr = nil
		return
	}
	for _, inf := range interfaces {
		infAddresses, err := inf.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range infAddresses {
			if _, ipNet, err := net.ParseCIDR(addr.String()); err == nil {
				if ipNet.Contains(t.IPAddr) {
					t.Location = Local
					if arpCli, err := arp.Dial(&inf); err == nil {
						err = arpCli.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
						if err != nil {
							colorFmt.Warnf("%s %s: Error setting read timeout for arp request. Skipping mac lookup...\n",
								symbols.INFO, t.IPAddr.String())
							t.MACAddr = nil
							_ = arpCli.Close()
							return
						}
						hwAddr, err := arpCli.Resolve(t.IPAddr)
						if err != nil || hwAddr.String() == "00:00:00:00:00:00" {
							t.MACAddr = nil
							_ = arpCli.Close()
							return
						}
						colorFmt.Infof("%s Found MAC address for target '%s' via arp request: %s\n",
							symbols.INFO, t.InitialTarget, hwAddr.String())
						t.MACAddr = hwAddr
						t.Status = Online
						_ = arpCli.Close()
						return
					} else {
						t.MACAddr = nil
						return
					}
				}
			}
		}
	}
	t.Location = Global
	t.MACAddr = nil
	return
}

func (t *Target) LookUpVendor() {
	if t.MACAddr != nil {
		vendorRes, err := macLookup.LookupVendor(t.MACAddr)
		if err != nil {
			t.Vendor = "N/A"
			return
		}
		t.Vendor = vendorRes.Company
		return
	}
	t.Vendor = "N/A"
}

func (t *Target) Ping(count int, privileged bool) (*ping.Statistics, error) {
	pinger, err := ping.NewPinger(t.IPAddr.String())
	if err != nil {
		t.Rtts = nil
		return nil, err
	}
	pinger.Timeout = time.Millisecond * 3000
	pinger.Count = count

	pinger.SetPrivileged(privileged || runtime.GOOS == "windows")

	pinger.Run()

	stats := pinger.Statistics()
	t.Rtts = stats.Rtts
	return stats, nil
}

func (t Target) AvgRtt() time.Duration {
	if len(t.Rtts) == 0 {
		return -1
	}
	var avgNS int64 = 1
	for _, rtt := range t.Rtts {
		avgNS += rtt.Nanoseconds()
	}
	avgNS /= int64(len(t.Rtts))
	return time.Duration(avgNS)
}

func (t *Target) IsHost() (bool, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false, err
	}
	for _, inf := range interfaces {
		infAddresses, err := inf.Addrs()
		if err != nil {
			continue
		}

		// Check if target ip is the host
		for _, addr := range infAddresses {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPAddr:
				ip = v.IP
				if t.IPAddr.Equal(ip) {
					t.Location = Local
					t.MACAddr = inf.HardwareAddr
					return true, nil
				}
			case *net.IPNet:
				ip = net.ParseIP(strings.Split(v.IP.String(), ":")[0])
				if t.IPAddr.Equal(ip) {
					t.Location = Local
					t.MACAddr = inf.HardwareAddr
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (t *Target) String() string {
	var mac string
	if t.MACAddr == nil {
		mac = "N/A"
	} else {
		mac = t.MACAddr.String()
	}

	return fmt.Sprintf(""+
		"~~~~~~~~~~~~~~~ TARGET INFO ~~~~~~~~~~~~~~~~~~~~~~\n"+
		"Target: %s | IP: %s | Hostname: %s\n"+
		"Avg Ping [%d send]: %v\n"+
		"Vendor: %s\n"+
		"MacAddress: %s\n"+
		"Network Location: %s\n"+
		"Status: %s\n"+
		"Ports:\n%s\n"+
		"~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~",
		t.InitialTarget, t.IPAddr, t.HostName,
		len(t.Rtts), t.AvgRtt(),
		t.Vendor,
		mac,
		t.Location,
		t.Status,
		t.Ports.Preview())
}

func (t *Target) ColorString() string {
	var mac string
	if t.MACAddr == nil && t.Location == Local && runtime.GOOS == "windows" {
		mac = colorFmt.Swarnf("N/A")
	} else if t.MACAddr == nil && t.Location == Local {
		mac = colorFmt.Sfatalf("N/A")
	} else if t.MACAddr == nil {
		mac = colorFmt.Ssuccessf("N/A")
	} else {
		mac = colorFmt.Ssuccessf(t.MACAddr.String())
	}

	var vendor string
	if t.Location == Local && t.Vendor != "" && t.Vendor != "N/A" {
		vendor = colorFmt.Ssuccessf(t.Vendor)
	} else if t.Location == Local {
		vendor = colorFmt.Sfatalf("N/A")
	} else if t.Location == Global {
		vendor = colorFmt.Ssuccessf("N/A")
	}

	var ping string
	avg := t.AvgRtt()
	if avg > 0 {
		ping = colorFmt.Ssuccessf("%v", avg)
	} else {
		ping = colorFmt.Sfatalf("%v", avg)
	}

	return fmt.Sprintf(""+
		"~~~~~~~~~~~~~~~ TARGET INFO ~~~~~~~~~~~~~~~~~~~~~~\n"+
		"Target: %s | IP: %s | Hostname: %s\n"+
		"Avg Ping [%d send]: %v\n"+
		"Vendor: %s\n"+
		"MacAddress: %s\n"+
		"Network Location: %s\n"+
		"Status: %s\n"+
		"Ports:\n%s\n"+
		"~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~",
		t.InitialTarget, t.IPAddr, t.HostName,
		len(t.Rtts), ping,
		vendor,
		mac,
		t.Location.ColorString(),
		t.Status.ColorString(),
		t.Ports.Preview())
}

func (ts TargetStatus) String() string {
	if ts == Online {
		return "ONLINE"
	} else if ts == OfflineFiltered {
		return "OFFLINE / FILTERED"
	} else if ts == Unknown {
		return "UNKNOWN"
	}
	return "N/A"
}

func (n NetworkLocation) String() string {
	if n == Local {
		return "LOCAL"
	} else if n == Global {
		return "GLOBAL"
	} else if n == UnknownLoc {
		return "UNKNOWN"
	}
	return "N/A"
}

func (ts TargetStatus) ColorString() string {
	if ts == Online {
		return colorFmt.Sopenf("ONLINE")
	} else if ts == OfflineFiltered {
		return colorFmt.Sclosedf("OFFLINE / FILTERED")
	} else if ts == Unknown {
		return colorFmt.Sfilteredf("UNKNOWN")
	}
	return "N/A"
}

func (n NetworkLocation) ColorString() string {
	if n == Local {
		return colorFmt.Sinfof("LOCAL")
	} else if n == Global {
		return colorFmt.Sinfof("GLOBAL")
	} else if n == UnknownLoc {
		return colorFmt.Swarnf("UNKNOWN")
	}
	return "N/A"
}

func octetsToTargets(octets [4][]int) []string {
	var targets []string
	for _, oc0 := range octets[0] {
		for _, oc1 := range octets[1] {
			for _, oc2 := range octets[2] {
				for _, oc3 := range octets[3] {
					targets = append(targets, fmt.Sprintf("%d.%d.%d.%d", oc0, oc1, oc2, oc3))
				}
			}
		}
	}
	return targets
}
