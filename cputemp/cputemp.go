package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	log.SetFlags(0)
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		log.Fatalln("Error establishing cache dir:", err)
	}
	symlink := filepath.Join(cacheDir, "cputemp", "cpu_temp")
	tempText, readErr := readFile(symlink)
	if errors.Is(readErr, os.ErrNotExist) {
		file, err := findTempFile()
		if err != nil {
			log.Fatalln("Error locating correct temperature file:", err)
		}
		if err := os.MkdirAll(filepath.Dir(symlink), 0o755); err != nil {
			log.Fatalln("Error creating cache dir:", err)
		}
		os.Remove(symlink) // best-effort
		if err := os.Symlink(file, symlink); err != nil {
			log.Fatalf("Error writing cache symlink %s->%s: %s", file, symlink, err)
		}
		tempText, readErr = readFile(symlink)
	}
	if readErr != nil {
		log.Fatalln("Error reading temperature file:", readErr)
	}

	temp, err := strconv.ParseInt(tempText, 10, 64)
	if err != nil {
		log.Fatalf("Error parsing contents of %s as an integer: %s", symlink, tempText)
	}
	fmt.Println(math.Round(float64(temp) / 1000))
}

func findTempFile() (string, error) {
	for _, opt := range []struct {
		deviceName string
		label      string
	}{
		{deviceName: "k10temp", label: "Tctl"},          // AMD Ryzen 9 3900X
		{deviceName: "coretemp", label: "Package id 0"}, // Intel Core i7-8565U
	} {
		path, err := resolveTempFile(opt.deviceName, opt.label)
		if errors.Is(err, errTempFileNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("temp file not found in any of the known locations")
}

var errTempFileNotFound = errors.New("temp file not found")

func resolveTempFile(deviceName, label string) (string, error) {
	dirs, err := filepath.Glob("/sys/class/hwmon/hwmon*")
	if err != nil {
		return "", err
	}
	var dir string
	for _, d := range dirs {
		name, err := readFile(filepath.Join(d, "name"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if name == deviceName {
			dir = d
			break
		}
	}
	if dir == "" {
		return "", errTempFileNotFound
	}
	labels, err := filepath.Glob(filepath.Join(dir, "temp*_label"))
	if err != nil {
		return "", err
	}
	for _, f := range labels {
		l, err := readFile(f)
		if err != nil {
			return "", err
		}
		if l == label {
			return filepath.EvalSymlinks(strings.TrimSuffix(f, "_label") + "_input")
		}
	}
	return "", fmt.Errorf("no temp file labeled %q located for device %q", label, deviceName)
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(b)), nil
}
