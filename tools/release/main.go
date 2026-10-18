// Command release cross-builds tazzy and packages it:
//
//	go run ./tools/release -version 0.1.0
//
// For each target it writes dist/tazzy_<v>_<os>_<arch>/tazzy[.exe] (stripped,
// -trimpath, CGO off), an archive next to it (.zip for windows, .tar.gz
// otherwise) holding the binary plus README.md (and LICENSE when present),
// and finally dist/SHA256SUMS over the archives. Stdlib only, so it runs
// the same on Windows, macOS and Linux without make.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var targets = []struct{ goos, goarch string }{
	{"windows", "amd64"}, {"windows", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
}

// Files shipped next to the binary when they exist.
var extras = []string{"README.md", "LICENSE"}

func main() {
	version := flag.String("version", "", "version to stamp into main.version (required, e.g. 0.1.0)")
	out := flag.String("out", "dist", "output directory (emptied first)")
	flag.Parse()
	if *version == "" {
		fmt.Fprintln(os.Stderr, "release: -version is required")
		os.Exit(2)
	}
	if err := release(strings.TrimPrefix(*version, "v"), *out); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func release(version, out string) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(root, out)
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var sums strings.Builder
	for _, t := range targets {
		name := fmt.Sprintf("tazzy_%s_%s_%s", version, t.goos, t.goarch)
		bin := "tazzy"
		if t.goos == "windows" {
			bin += ".exe"
		}
		binPath := filepath.Join(out, name, bin)
		cmd := exec.Command("go", "build", "-trimpath",
			"-ldflags", "-s -w -X main.version="+version,
			"-o", binPath, "./cmd/tazzy")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", t.goos, t.goarch, err)
		}

		files := map[string]string{bin: binPath} // archive name -> source
		order := []string{bin}
		for _, e := range extras {
			if _, err := os.Stat(filepath.Join(root, e)); err == nil {
				files[e] = filepath.Join(root, e)
				order = append(order, e)
			}
		}
		archive := filepath.Join(out, name+".tar.gz")
		write := writeTarGz
		if t.goos == "windows" {
			archive, write = filepath.Join(out, name+".zip"), writeZip
		}
		if err := write(archive, name, order, files); err != nil {
			return fmt.Errorf("archive %s: %w", archive, err)
		}
		sum, err := sha256File(archive)
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", sum, filepath.Base(archive))

		bs, as := size(binPath), size(archive)
		fmt.Printf("%-8s %-6s binary %5.2f MB  %s %5.2f MB\n", t.goos, t.goarch, mb(bs), strings.TrimPrefix(filepath.Base(archive), name), mb(as))
	}
	if err := os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", out)
	return nil
}

// moduleRoot is the directory of the main module's go.mod.
func moduleRoot() (string, error) {
	b, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(b))
	if mod == "" || mod == os.DevNull {
		return "", fmt.Errorf("run this inside the tazzyronth module")
	}
	return filepath.Dir(mod), nil
}

// Archive entries live under a top-level directory named after the archive;
// the modification time is fixed so rebuilding the same version is stable.
var stamp = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func writeZip(path, dir string, order []string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, name := range order {
		h := &zip.FileHeader{Name: dir + "/" + name, Method: zip.Deflate, Modified: stamp}
		h.SetMode(0o644)
		if isBinary(name) {
			h.SetMode(0o755)
		}
		w, err := zw.CreateHeader(h)
		if err == nil {
			err = copyFile(w, files[name])
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeTarGz(path, dir string, order []string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range order {
		mode := int64(0o644)
		if isBinary(name) {
			mode = 0o755
		}
		h := &tar.Header{Name: dir + "/" + name, Mode: mode, Size: size(files[name]), ModTime: stamp, Format: tar.FormatPAX}
		err := tw.WriteHeader(h)
		if err == nil {
			err = copyFile(tw, files[name])
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	for _, c := range []io.Closer{tw, gz, f} {
		if err := c.Close(); err != nil {
			return err
		}
	}
	return nil
}

func isBinary(name string) bool { return strings.HasPrefix(name, "tazzy") }

func copyFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }
