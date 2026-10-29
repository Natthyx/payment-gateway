// bcryptbench measures one bcrypt cost-12 compare, the work the gateway does
// on every authenticated request (internal/merchant/service.go). Run inside
// the Docker VM so it sees the same CPUs as the app.
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	secret := []byte("0123456789abcdef0123456789abcdef0123456789abcdef")
	hash, err := bcrypt.GenerateFromPassword(secret, 12)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	const n = 20
	ds := make([]time.Duration, n)
	for i := range ds {
		t := time.Now()
		_ = bcrypt.CompareHashAndPassword(hash, secret)
		ds[i] = time.Since(t)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	fmt.Printf("GOMAXPROCS=%d NumCPU=%d\n", runtime.GOMAXPROCS(0), runtime.NumCPU())
	fmt.Printf("sequential cost-12 compare, n=%d: min=%.1fms median=%.1fms max=%.1fms\n",
		n, ms(ds[0]), ms(ds[n/2]), ms(ds[n-1]))

	workers := runtime.NumCPU()
	const per = 10
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = bcrypt.CompareHashAndPassword(hash, secret)
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)
	fmt.Printf("parallel: %d workers x %d compares in %.2fs = %.1f compares/s\n",
		workers, per, el.Seconds(), float64(workers*per)/el.Seconds())
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
