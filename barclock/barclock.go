package main

import (
	"flag"
	"fmt"
	"time"
)

func main() {
	secs := flag.Bool("secs", false, "Use second resolution")
	flag.Parse()

	resolution := time.Minute
	if *secs {
		resolution = time.Second
	}
	for {
		t := time.Now().Truncate(resolution)
		printTime(t, resolution)
		t = t.Add(resolution)
		time.Sleep(time.Until(t))
	}
}

func printTime(t time.Time, res time.Duration) {
	tailFormat := "15:04 MST"
	if res == time.Second {
		tailFormat = "15:04:05 MST"
	}
	localFormat := "Jan 2 " + tailFormat
	utc := t.UTC()
	utcFormat := localFormat
	if t.Day() == utc.Day() {
		utcFormat = tailFormat
	}
	fmt.Printf("%s • %s\n", t.Format(localFormat), utc.Format(utcFormat))
}
