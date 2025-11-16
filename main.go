package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	ar "github.com/erikgeiser/ar"
	"github.com/schollz/progressbar/v3"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// --- Configuration ---
const MaxMemoryUsage = 2 * 1024 * 1024 * 1024 // 2GB RAM Limit

// --- Structures ---

// VirtualFile acts as the bridge between the extracted tar and the final zip
type VirtualFile struct {
	Name     string
	Data     []byte
	DiskPath string
	Mode     int64
	ModTime  time.Time
	IsDir    bool
	IsLink   bool
	LinkDest string
}

// Plist structures for parsing Info.plist (Matches Swift's Info.plist reading)
type Plist struct {
	Dict PlistDict `xml:"dict"`
}
type PlistDict struct {
	Keys   []string `xml:"key"`
	String []string `xml:"string"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: deb-to-ipa <path-to-deb-file>")
		os.Exit(1)
	}

	debPath := os.Args[1]
	fmt.Println("📱 DebToIPA")
	fmt.Println("------------------------------------------")

	start := time.Now()

	// Matches Swift: ContentView.swift -> convert(url:)
	err := convert(debPath)
	if err != nil {
		fmt.Printf("\n❌ Error: %v\n", err)
		// Matches Swift: ConversionError handling
		os.Exit(1)
	}

	fmt.Printf("\n✅ Successfully converted to IPA in %s!\n", time.Since(start).Round(time.Second))
}

func convert(debPath string) error {
	// Matches Swift: DebToIPA.swift -> extractDeb() -> Reading .deb
	fmt.Println("=> [1/5] Opening Deb Archive...")
	debFile, err := os.Open(debPath)
	if err != nil {
		return fmt.Errorf("no permission or file not found: %w", err)
	}
	defer debFile.Close()

	arReader, err := ar.NewReader(debFile)
	if err != nil {
		return fmt.Errorf("invalid deb archive: %w", err)
	}

	// Matches Swift: "data.tar" detection loop
	var dataTar io.Reader
	foundData := false

	for {
		header, err := arReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if strings.HasPrefix(header.Name, "data.tar") {
			foundData = true
			fmt.Printf("=> [2/5] Found %s. Decompressing...\n", header.Name)

			// Matches Swift: DecompressionMethod switch (lzma, gz, bzip2, xz)
			switch {
			case strings.HasSuffix(header.Name, ".gz"):
				dataTar, err = gzip.NewReader(arReader)
			case strings.HasSuffix(header.Name, ".lzma"):
				dataTar, err = lzma.NewReader(arReader)
			case strings.HasSuffix(header.Name, ".bzip2"):
				dataTar = bzip2.NewReader(arReader)
			case strings.HasSuffix(header.Name, ".xz"):
				dataTar, err = xz.NewReader(arReader)
			default:
				// Matches Swift: ConversionError.unsupportedCompression
				return fmt.Errorf("unsupported compression method: %s", header.Name)
			}
			if err != nil {
				return fmt.Errorf("decompression failed: %w", err)
			}
			break
		}
	}

	// Matches Swift: ConversionError.noDataFound
	if !foundData {
		return fmt.Errorf("data.tar not found in deb")
	}

	// --- Extraction Logic ---
	// Unlike Swift which extracts to disk immediately, we extract to RAM/Spillover
	// to perform the same logic but faster and cross-platform.

	tarReader := tar.NewReader(dataTar)

	// Matches Swift: cleanup() logic (via defer)
	tempDir, err := os.MkdirTemp("", "ipa-spill")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir) // This handles the "Clean after running" toggle logic

	var files []*VirtualFile
	var currentRamUsage int64 = 0
	var totalSize int64 = 0

	// State for app detection
	var appDirPrefix string
	var infoPlistData []byte // To parse BundleID/ExecName

	fmt.Print("=> [3/5] Extracting and Analyzing Files... ")

	fileCount := 0
	spillCount := 0

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}

		fileCount++
		if fileCount%100 == 0 {
			fmt.Printf("\r=> [3/5] Analyzing Files... (%d scanned)", fileCount)
		}

		// Matches Swift: Checking for "Applications/" folder structure
		// We also support root-level .app (common in tweaked debs)
		if appDirPrefix == "" {
			if idx := strings.Index(header.Name, ".app/"); idx != -1 {
				// Capture "Applications/MyApp.app/" or "./MyApp.app/"
				appDirPrefix = header.Name[:idx+5]
			}
		}

		vFile := &VirtualFile{
			Name:    header.Name,
			Mode:    header.Mode,
			// **FIXED HERE**: Removed the "Size" field
			ModTime: header.ModTime,
			IsDir:   header.Typeflag == tar.TypeDir,
		}

		if header.Typeflag == tar.TypeSymlink {
			// Matches Swift: entry.info.type == .symbolicLink
			vFile.IsLink = true
			vFile.LinkDest = header.Linkname
			files = append(files, vFile)
		} else if header.Typeflag == tar.TypeReg {
			// Matches Swift: entry.info.type == .regular
			totalSize += header.Size

			// RAM vs Disk decision
			var data []byte
			if currentRamUsage+header.Size < MaxMemoryUsage {
				data, err = io.ReadAll(tarReader)
				if err != nil {
					return err
				}
				vFile.Data = data
				currentRamUsage += int64(len(data))
			} else {
				// Spill to disk (simulating Swift's extract to tempDir)
				spillCount++
				tempPath := filepath.Join(tempDir, fmt.Sprintf("spill_%d", spillCount))
				f, err := os.Create(tempPath)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, tarReader)
				f.Close()
				vFile.DiskPath = tempPath
			}

			// Capture Info.plist for parsing (Matches Swift's logic to read Plist)
			if strings.HasSuffix(header.Name, "Info.plist") && len(data) > 0 {
				infoPlistData = data
			}

			files = append(files, vFile)
		} else if header.Typeflag == tar.TypeDir {
			// Matches Swift: entry.info.type == .directory
			files = append(files, vFile)
		}
	}
	fmt.Println()

	// Matches Swift: ConversionError.unsupportedApp
	if appDirPrefix == "" {
		return fmt.Errorf("unsupported app: could not find .app directory inside deb")
	}

	// --- Metadata Parsing (Matches Swift: SavedIpa struct logic) ---
	fmt.Println("=> [4/5] Parsing App Metadata...")

	executableName := ""
	bundleID := "Unknown"
	version := "Unknown"

	if len(infoPlistData) > 0 {
		var plist Plist
		if err := xml.Unmarshal(infoPlistData, &plist); err == nil {
			// Iterate keys to find values
			for i, key := range plist.Dict.Keys {
				if i >= len(plist.Dict.String) {
					break
				}

				if key == "CFBundleExecutable" {
					executableName = plist.Dict.String[i]
				}
				if key == "CFBundleIdentifier" {
					bundleID = plist.Dict.String[i]
				}
				if key == "CFBundleVersion" || key == "CFBundleShortVersionString" {
					version = plist.Dict.String[i]
				}
			}
		}
	}

	// Fallback: guess executable name from folder name if Plist failed
	cleanAppPrefix := filepath.ToSlash(appDirPrefix) // e.g. "./Applications/MyApp.app/"
	appNameFolder := path.Base(cleanAppPrefix)       // "MyApp.app"
	if executableName == "" {
		executableName = strings.TrimSuffix(appNameFolder, ".app")
	}

	fmt.Printf("   Name: %s\n   ID:   %s\n   Ver:  %s\n   Exec: %s\n",
		appNameFolder, bundleID, version, executableName)

	// --- IPA Construction (Matches Swift: Create .ipa archive) ---
	ipaPath := strings.TrimSuffix(debPath, ".deb") + ".ipa"
	fmt.Println("=> [5/5] Zipping Payload...")

	ipaFile, err := os.Create(ipaPath)
	if err != nil {
		return err
	}
	defer ipaFile.Close()

	zipWriter := zip.NewWriter(ipaFile)
	defer zipWriter.Close()

	bar := progressbar.DefaultBytes(totalSize, "Writing IPA")

	for _, vf := range files {
		cleanName := filepath.ToSlash(vf.Name)

		// Filter: Only process files inside the detected .app folder
		if !strings.HasPrefix(cleanName, cleanAppPrefix) {
			continue
		}

		// Logic: Relativize path.
		// "Applications/MyApp.app/Info.plist" -> "Info.plist"
		relPath := strings.TrimPrefix(cleanName, cleanAppPrefix)

		// Construct Payload path: "Payload/MyApp.app/Info.plist"
		finalPath := path.Join("Payload", appNameFolder, relPath)

		if vf.IsDir {
			finalPath += "/"
		}

		header := &zip.FileHeader{
			Name:     finalPath,
			Method:   zip.Deflate,
			Modified: vf.ModTime,
		}

		// --- PERMISSION FIXES (Crucial for Ldid/TrollStore) ---
		// This is the new, correct logic that mimics 7-Zip and the Swift Zip library.

		// Get the 9-bit permission (e.g., 0755, 0644) from the tar header
		perms := os.FileMode(vf.Mode) & 0777
		var unixFileType uint32

		// 1. Handle Symlinks
		if vf.IsLink {
			header.Method = zip.Store
			unixFileType = 0xA000 // S_IFLNK (Symbolic Link)
			perms = 0777         // Symlinks are typically 777
			header.SetMode(os.ModeSymlink | perms)

			// 2. Handle Directories
		} else if vf.IsDir {
			header.Method = zip.Store
			unixFileType = 0x4000 // S_IFDIR (Directory)
			if perms == 0 {
				perms = 0755
			} // Ensure dirs are at least 0755
			header.SetMode(os.ModeDir | perms)

			// 3. Handle Regular Files
		} else {
			unixFileType = 0x8000 // S_IFREG (Regular File)

			// Check if this file is the Main Binary
			isMainBinary := false
			if path.Base(finalPath) == executableName {
				isMainBinary = true
			}

			// 3a. Force Executable Permissions
			// The .deb might have 0644. iOS NEEDS 0755 for the binary.
			if isMainBinary || strings.HasSuffix(finalPath, ".dylib") || strings.Contains(finalPath, "/bin/") {
				perms = 0755 // rwxr-xr-x
			} else if perms == 0 {
				perms = 0644 // Default for non-exec files
			}

			// 3b. Optimization: Store binary uncompressed
			if isMainBinary {
				header.Method = zip.Store
			}

			header.SetMode(perms) // SetMode for regular files just takes perms
		}

		// **THE FIX**: Set the Unix External Attribute (mode << 16)
		// This tells iOS/ldid that this file is a link/dir/executable.
		header.ExternalAttrs = (unixFileType | uint32(perms)) << 16

		w, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}

		if vf.IsLink {
			w.Write([]byte(vf.LinkDest))
		} else if !vf.IsDir {
			if vf.DiskPath != "" {
				f, _ := os.Open(vf.DiskPath)
				io.Copy(io.MultiWriter(w, bar), f)
				f.Close()
			} else {
				io.Copy(io.MultiWriter(w, bar), bytes.NewReader(vf.Data))
			}
		}
	}

	return nil
}
