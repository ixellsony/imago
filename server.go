package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"

	"go.etcd.io/bbolt"
)

const (
	Port        = ":2727"
	BlockSize   = 2 * 1024 * 1024 // On remet 2 Mo car ça marchait bien
	StorageFile = "backup.raw"
	DbFile      = "manifest.db"
	BucketName  = "BlockHashes"
)

type Request struct {
	Type  string // "CHECK", "DATA", ou "ZERO"
	Index int64
	Hash  string
	Size  int
}

type Response struct {
	Action string
}

func main() {
	// DB
	db, err := bbolt.Open(DbFile, 0600, nil)
	if err != nil { log.Fatal(err) }
	defer db.Close()
	db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(BucketName))
		return err
	})

	// Storage
	storeFile, err := os.OpenFile(StorageFile, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil { log.Fatal(err) }
	enableSparseFile(storeFile)
	defer storeFile.Close()

	// Network
	listener, err := net.Listen("tcp", Port)
	if err != nil { log.Fatal(err) }
	fmt.Printf("SERVEUR V3: En écoute sur %s\n", Port)

	for {
		conn, err := listener.Accept()
		if err != nil { continue }
		go handleClient(conn, db, storeFile)
	}
}

func handleClient(conn net.Conn, db *bbolt.DB, storeFile *os.File) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	encoder := json.NewEncoder(conn)

	fmt.Println("Client connecté.")

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF { fmt.Println("Déconnexion:", err) }
			return
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil { return }

		// --- CAS 1 : CHECK (Déduplication) ---
		if req.Type == "CHECK" {
			var action string
			db.View(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte(BucketName))
				stored := b.Get(itob(req.Index))
				if stored != nil && string(stored) == req.Hash {
					action = "SKIP"
				} else {
					action = "SEND"
				}
				return nil
			})
			encoder.Encode(Response{Action: action})

		// --- CAS 2 : ZERO (C'est du vide, on saute sans écrire) ---
		} else if req.Type == "ZERO" {
			// On déplace juste le curseur (crée un trou dans le fichier)
			offset := req.Index * int64(BlockSize)
			
			// Astuce: Seek suffit pour le sparse file, mais il faut s'assurer
			// que le fichier grandit.
			storeFile.Seek(offset, 0)
			// On n'écrit rien physiquement, mais on enregistre le hash "vide" en DB
			// pour les prochains backups incrémentaux
			db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte(BucketName))
				return b.Put(itob(req.Index), []byte(req.Hash))
			})
			
			encoder.Encode(Response{Action: "ACK"})

		// --- CAS 3 : DATA (Vraies données) ---
		} else if req.Type == "DATA" {
			buf := make([]byte, req.Size)
			if _, err := io.ReadFull(reader, buf); err != nil {
				return
			}

			offset := req.Index * int64(BlockSize)
			storeFile.Seek(offset, 0)
			storeFile.Write(buf)

			db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte(BucketName))
				return b.Put(itob(req.Index), []byte(req.Hash))
			})
			encoder.Encode(Response{Action: "ACK"})
		}
	}
}

func itob(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func enableSparseFile(file *os.File) error {
	const FSCTL_SET_SPARSE = 0x000900C4
	var bytesReturned uint32
	return syscall.DeviceIoControl(syscall.Handle(file.Fd()), FSCTL_SET_SPARSE, nil, 0, nil, 0, &bytesReturned, nil)
}
