package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var serviceByPort = map[int]string{
	22: "ssh", 80: "http", 443: "https", 3306: "mysql",
	5432: "postgres", 6379: "redis", 8080: "http", 8443: "https", 9000: "http",
}

func guessService(port int) string {
	if s, ok := serviceByPort[port]; ok {
		return s
	}
	return "tcp"
}

// expandCIDR lists host IPs in a CIDR (or a bare IP as /32). Refuses ranges wider than /22 (>1024).
func expandCIDR(cidr string) ([]string, error) {
	if !strings.Contains(cidr, "/") {
		if p := net.ParseIP(cidr); p != nil {
			return []string{p.String()}, nil
		}
		return nil, fmt.Errorf("invalid IP: %s", cidr)
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %s", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	if bits-ones > 10 {
		return nil, fmt.Errorf("CIDR too wide (max /22): %s", cidr)
	}
	var out []string
	for cur := ip.Mask(ipnet.Mask); ipnet.Contains(cur); incIP(cur) {
		out = append(out, cur.String())
	}
	return out, nil
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func parsePorts(s string) []int {
	if strings.TrimSpace(s) == "" {
		return []int{22, 80, 443, 3306, 5432, 6379, 8080, 8443, 9000}
	}
	seen := map[int]bool{}
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 && n < 65536 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func scanTarget(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// runDiscovery scans cidr x ports (bounded) and reports open services. One pass.
func runDiscovery(client *Client, cidr string, ports []int, concurrency int) {
	hosts, err := expandCIDR(cidr)
	if err != nil {
		log.Printf("discovery: %v", err)
		return
	}
	type job struct {
		host string
		port int
	}
	jobs := make(chan job)
	var mu sync.Mutex
	var found []DiscoveredItem
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if scanTarget(j.host, j.port, 1500*time.Millisecond) {
					mu.Lock()
					found = append(found, DiscoveredItem{Host: j.host, Port: j.port, ServiceGuess: guessService(j.port)})
					mu.Unlock()
				}
			}
		}()
	}
	for _, h := range hosts {
		for _, p := range ports {
			jobs <- job{h, p}
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("discovery: scanned %d hosts x %d ports, %d open", len(hosts), len(ports), len(found))
	if len(found) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Discovered(ctx, found); err != nil {
		log.Printf("discovery report error: %v", err)
	}
}
