package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	ar "github.com/erikgeiser/ar"
	"github.com/schollz/progressbar/v3"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Config: 2GB RAM Limit
const MaxMemoryUsage = 2 * 1024 * 1024 * 1024 

// VirtualFile represents a file that could be in RAM or on Disk
type VirtualFile struct {
	Name     string      // Relative path in the IPA
	Data     []byte      // Used if in RAM
	DiskPath string      // Used if spilled to Disk
	Mode     int64       // File permissions
	Size     int64       
	ModTime  time.Time
	IsDir    bool
	IsLink   bool
	LinkDest string      // For symlinks
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: deb-to-ipa <path-to-deb-file>")
		os.Exit(1)
	}

	debPath := os.Args[1]
	fmt.Println("📦 Starting optimized RAM conversion for:", filepath.Base(debPath))
	
	start := time.Now()
	err := convert(debPath)
	if err != nil {
		fmt.Printf("\n❌ Error converting %s: %v\n", debPath, err)
		os.Exit(1)
	}

	fmt.Printf("\n✅ Successfully converted to IPA in %s!\n", time.Since(start).Round(time.Second))
}

func convert(debPath string) error {
	// 1. Open the .deb file
	fmt.Println("=> [1/4] Reading DEB archive...")
	debFile, err := os.Open(debPath)
	if err != nil {
		return fmt.Errorf("failed to open deb file: %w", err)
	}
	defer debFile.Close()

	arReader, err := ar.NewReader(debFile)
	if err != nil {
		return fmt.Errorf("failed to create ar reader: %w", err)
	}

	// 2. Find decompression stream
	var dataTar io.Reader
	foundData := false
	
	for {
		header, err := arReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read ar header: %w", err)
		}

		if strings.HasPrefix(header.Name, "data.tar") {
			foundData = true
			fmt.Printf("=> [2/4] Decompressing %s (Stream Mode)...\n", header.Name)
			
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
				return fmt.Errorf("unsupported compression: %s", header.Name)
			}
			if err != nil {
				return fmt.Errorf("decompression init failed: %w", err)
			}
			break
		}
	}

	if !foundData {
		return fmt.Errorf("data.tar not found")
	}

	// 3. Extract to Memory (with Disk Spillover)
	tarReader := tar.NewReader(dataTar)
	
	// Temp dir for spillover only
	tempDir, err := os.MkdirTemp("", "ipa-spillover")
	if err != nil {
		return fmt.Errorf("failed to create spillover dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var files []*VirtualFile
	var currentRamUsage int64 = 0
	var totalUncompressedSize int64 = 0
	fileCount := 0
	spillCount := 0

	fmt.Print("=> [3/4] Processing files into Memory... ")

	appDirPrefix := ""

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}

		fileCount++
		if fileCount % 50 == 0 {
			fmt.Printf("\r=> [3/4] Processing files... (%d processed | RAM: %d MB)", 
				fileCount, currentRamUsage/1024/1024)
		}

		// Identify the .app directory
		if appDirPrefix == "" && strings.HasSuffix(header.Name, ".app/") {
			appDirPrefix = header.Name
		}

		vFile := &VirtualFile{
			Name:    header.Name,
			Mode:    header.Mode,
			Size:    header.Size,
			ModTime: header.ModTime,
			IsDir:   header.Typeflag == tar.TypeDir,
		}

		if header.Typeflag == tar.TypeSymlink {
			vFile.IsLink = true
			vFile.LinkDest = header.Linkname
			// Symlinks are tiny, always keep in RAM
			files = append(files, vFile)
			continue
		}

		if header.Typeflag == tar.TypeReg {
			totalUncompressedSize += header.Size
			
			// Check Memory Limit
			if currentRamUsage+header.Size < MaxMemoryUsage {
				// STORE IN RAM
				data, err := io.ReadAll(tarReader)
				if err != nil {
					return err
				}
				vFile.Data = data
				currentRamUsage += int64(len(data))
			} else {
				// SPILL TO DISK
				spillCount++
				tempPath := filepath.Join(tempDir, fmt.Sprintf("spill_%d", spillCount))
				f, err := os.Create(tempPath)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, tarReader)
				f.Close()
				if err != nil {
					return err
				}
				vFile.DiskPath = tempPath
			}
			files = append(files, vFile)
		} else if header.Typeflag == tar.TypeDir {
			files = append(files, vFile)
		}
	}
	
	fmt.Printf("\r=> [3/4] Processed %d files. (RAM: %d MB | Disk Spills: %d)\n", 
		fileCount, currentRamUsage/1024/1024, spillCount)

	if appDirPrefix == "" {
		return fmt.Errorf(".app directory not found inside tar")
	}

	// 4. Write ZIP (IPA)
	ipaPath := strings.TrimSuffix(debPath, ".deb") + ".ipa"
	fmt.Println("=> [4/4] Constructing IPA...")

	ipaFile, err := os.Create(ipaPath)
	if err != nil {
		return err
	}
	defer ipaFile.Close()

	zipWriter := zip.NewWriter(ipaFile)
	defer zipWriter.Close()

	bar := progressbar.DefaultBytes(totalUncompressedSize, "Zipping")

	for _, vf := range files {
		// We need to re-map the path from "./Start.app/Info.plist" to "Payload/Start.app/Info.plist"
		// But we must ensure we are inside the appDir identified earlier
		
		// Normalize paths
		cleanName := vf.Name
		// Only include files that are part of the identified .app tree
		if !strings.HasPrefix(cleanName, appDirPrefix) {
			continue 
		}
		
		// Construct Payload path
		// If appDirPrefix is "./Payload/X.app/", filepath.Rel helps us strip the prefix if needed,
		// but usually debs are "./Applications/X.app" or just "./X.app"
		// We want "Payload/X.app/..."
		
		// Simple logic: Retain the structure starting from the .app folder
		relInsideApp := strings.TrimPrefix(cleanName, appDirPrefix)
		finalPath := filepath.Join("Payload", filepath.Base(appDirPrefix), relInsideApp)
		
		if vf.IsDir {
			finalPath += "/"
		}

		header := &zip.FileHeader{
			Name:     finalPath,
			Method:   zip.Deflate,
			Modified: vf.ModTime,
		}
		
		if vf.IsLink {
			header.Method = zip.Store // Symlinks shouldn't be compressed
			// Start bitwise logic for symlink attributes in Zip
			header.SetMode(os.FileMode(vf.Mode) | os.ModeSymlink)
		} else if vf.IsDir {
			header.Method = zip.Store
		} else {
			// Preserve executable bits
			header.SetMode(os.FileMode(vf.Mode))
		}

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}

		if vf.IsLink {
			// Write the link destination as the file body
			writer.Write([]byte(vf.LinkDest))
		} else if !vf.IsDir {
			// Write Content
			if vf.DiskPath != "" {
				// Read from Spillover
				f, err := os.Open(vf.DiskPath)
				if err != nil {
					return err
				}
				io.Copy(io.MultiWriter(writer, bar), f)
				f.Close()
				// We can delete the temp file immediately to free space if we wanted, 
				// but defer cleanup is safer.
			} else {
				// Read from RAM
				io.Copy(io.MultiWriter(writer, bar), bytes.NewReader(vf.Data))
			}
		}
	}

	return nil
}