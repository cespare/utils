package main

import (
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const basePath = "/sys/class/backlight/intel_backlight"

func read(name string) int64 {
	b, err := ioutil.ReadFile(filepath.Join(basePath, name))
	if err != nil {
		log.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		log.Fatal(err)
	}
	return n
}

func write(name string, n int64) {
	s := strconv.FormatInt(n, 10)
	f, err := os.OpenFile(filepath.Join(basePath, name), os.O_TRUNC|os.O_WRONLY, 0)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := f.Write([]byte(s)); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(0)
	if len(os.Args) > 2 {
		log.Fatal("usage: intelbacklight [delta]")
	}
	max := read("max_brightness")
	cur := read("brightness")
	if len(os.Args) == 1 {
		pct := float64(cur) / float64(max) * 100
		log.Printf("max: %d, current: %d (%.1f%%)", max, cur, pct)
		return
	}
	delta, err := strconv.ParseFloat(os.Args[1], 64)
	if err != nil {
		log.Fatalf("Bad delta %q: %s", os.Args[1], err)
	}
	deltaAbs := int64(delta / 100 * float64(max))
	newVal := cur + deltaAbs
	if newVal < 0 {
		newVal = 0
	}
	if newVal > max {
		newVal = max
	}
	log.Printf("Changing %d -> %d (delta: %d)", cur, newVal, deltaAbs)
	write("brightness", newVal)
}
