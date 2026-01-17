package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	ServerAddr = "192.168.1.10:2727"
	BlockSize  = 2 * 1024 * 1024 // 2 Mo (Performance maximale)
	Volume     = "C:\\"
)

type Request struct {
	Type  string
	Index int64
	Hash  string
	Size  int
}

type Response struct {
	Action string
}

func main() {
	totalSize := getDiskSize("C:")
	totalBlocks := totalSize / int64(BlockSize)
	
	fmt.Println("CLIENT: Snapshot VSS...")
	vssPath, err := createShadowCopy(Volume)
	if err != nil { log.Fatalf("Erreur VSS: %v", err) }
	
	diskFile, err := os.Open(vssPath)
	if err != nil { log.Fatal(err) }
	defer diskFile.Close()

	fmt.Println("CLIENT: Connexion...")
	conn, err := net.Dial("tcp", ServerAddr)
	if err != nil { log.Fatal(err) }
	defer conn.Close()

	serverReader := json.NewDecoder(conn)
	buffer := make([]byte, BlockSize)
	zeroBlock := make([]byte, BlockSize) // Bloc rempli de zéros
	var blockIndex int64 = 0
	
	startTime := time.Now()
	lastPrint := time.Now()

	fmt.Printf("Démarrage Backup V3 (%.2f Go)\n", float64(totalSize)/1024/1024/1024)

	for {
		n, err := diskFile.Read(buffer)
		if n == 0 { break }
		if err != nil && err != io.EOF { log.Fatal(err) }

		chunk := buffer[:n]
		
		// Calcul Hash (nécessaire même pour le vide pour la DB)
		hashSum := sha256.Sum256(chunk)
		hashStr := hex.EncodeToString(hashSum[:])

		// --- NOUVELLE LOGIQUE ZERO ---
		if bytes.Equal(chunk, zeroBlock[:n]) {
			// C'est vide ! On prévient le serveur explicitement
			// Ça garde la connexion vivante et c'est très léger (pas de données brutes)
			err := sendJSON(conn, Request{Type: "ZERO", Index: blockIndex, Hash: hashStr})
			if err != nil { log.Fatal("ZERO Err:", err) }
			
			// On attend l'ACK pour être sûr que le serveur suit
			var resp Response
			if err := serverReader.Decode(&resp); err != nil { log.Fatal("ZERO ACK Err:", err) }
			
			// Affichage spécifique
			if time.Since(lastPrint) > 500*time.Millisecond {
				printProgress(blockIndex, totalBlocks, startTime, "ZERO-MARK")
				lastPrint = time.Now()
			}
			blockIndex++
			continue
		}

		// --- LOGIQUE NORMALE (NON VIDE) ---
		
		// 1. CHECK
		if err := sendJSON(conn, Request{Type: "CHECK", Index: blockIndex, Hash: hashStr}); err != nil {
			log.Fatal("CHECK Err:", err)
		}

		var resp Response
		if err := serverReader.Decode(&resp); err != nil {
			log.Fatal("CHECK Resp Err:", err)
		}

		status := "SKIP"
		if resp.Action == "SEND" {
			status = "SEND"
			// 2. DATA
			if err := sendJSON(conn, Request{Type: "DATA", Index: blockIndex, Hash: hashStr, Size: n}); err != nil {
				log.Fatal("HEADER Err:", err)
			}
			if _, err := conn.Write(chunk); err != nil {
				log.Fatal("DATA Err:", err)
			}
			// 3. ACK
			if err := serverReader.Decode(&resp); err != nil {
				log.Fatal("DATA ACK Err:", err)
			}
		}

		blockIndex++
		if time.Since(lastPrint) > 500*time.Millisecond {
			printProgress(blockIndex, totalBlocks, startTime, status)
			lastPrint = time.Now()
		}

		if err == io.EOF { break }
	}
	printProgress(blockIndex, totalBlocks, startTime, "FINI")
}

func printProgress(current, total int64, start time.Time, lastAction string) {
	if total == 0 { total = 1 }
	percent := float64(current) / float64(total) * 100
	duration := time.Since(start)
	processedMB := float64(current * int64(BlockSize)) / 1024 / 1024
	speed := processedMB / duration.Seconds()
	
	remainingBlocks := total - current
	var eta time.Duration
	if current > 0 {
		etaSeconds := float64(remainingBlocks) * (duration.Seconds() / float64(current))
		eta = time.Duration(int64(etaSeconds) * 1000 * 1000 * 1000)
	}

	width := 20
	completed := int(float64(width) * (float64(current) / float64(total)))
	if completed > width { completed = width }
	bar := strings.Repeat("=", completed) + strings.Repeat("-", width-completed)

	fmt.Printf("\r[%s] %5.1f%% | %-9s | %5.1f Mo/s | ETA: %v   ", bar, percent, lastAction, speed, eta.Round(time.Second))
}

func sendJSON(conn net.Conn, req Request) error {
	data, err := json.Marshal(req)
	if err != nil { return err }
	if _, err := conn.Write(data); err != nil { return err }
	if _, err := conn.Write([]byte("\n")); err != nil { return err }
	return nil
}

func createShadowCopy(volume string) (string, error) {
	cmdStr := fmt.Sprintf(`
		$s = (gwmi -List Win32_ShadowCopy).Create('%s', 'ClientAccessible')
		if ($s.ReturnValue -eq 0) {
			$shadow = gwmi Win32_ShadowCopy | ? { $_.ID -eq $s.ShadowID }
			Write-Host $shadow.DeviceObject
		} else {
			exit 1
		}
	`, volume)
	cmd := exec.Command("powershell", "-Command", cmdStr)
	out, err := cmd.CombinedOutput()
	if err != nil { return "", fmt.Errorf("%v", err) }
	return strings.TrimSpace(string(out)), nil
}

func getDiskSize(driveLetter string) int64 {
	drive := strings.TrimRight(driveLetter, "\\")
	cmdStr := fmt.Sprintf(`gwmi Win32_LogicalDisk -Filter "DeviceID='%s'" | select -ExpandProperty Size`, drive)
	cmd := exec.Command("powershell", "-Command", cmdStr)
	out, err := cmd.CombinedOutput()
	val, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil { return 100 * 1024 * 1024 * 1024 }
	return val
}
