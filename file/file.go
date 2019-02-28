package file

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	. "github.com/jackdoe/baxx/user"
	"github.com/jinzhu/gorm"
	"github.com/pierrec/lz4"
	"github.com/satori/go.uuid"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"time"
)

type FileOrigin struct {
	ID     uint64 `gorm:"primary_key"`
	Size   uint64 `gorm:"not null"`
	SHA256 string `gorm:"not null"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (fo *FileOrigin) FSPath() string {
	return locate(fo.SHA256)
}

type FileMetadata struct {
	ID uint64 `gorm:"primary_key"`

	UserID   uint64 `gorm:"not null"`
	TokenID  string `gorm:"not null"`
	Path     string `gorm:"not null"`
	Filename string `gorm:"not null"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

type FileVersion struct {
	ID             uint64 `gorm:"primary_key"`
	FileMetadataID uint64 `gorm:"not null" json:"-"`
	FileOriginID   uint64 `gorm:"not null" json:"-"`

	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func split(s string) (string, string) {
	s = filepath.Clean(s)
	name := filepath.Base(s)
	dir := filepath.Dir(s)
	return dir, name
}

func locate(f string) string {
	dir := path.Join("/", "tmp", "baxx")
	return path.Join(dir, f)
}

func saveUploadedFile(key string, body io.Reader) (string, int64, error) {
	sha := sha256.New()
	tee := io.TeeReader(body, sha)
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", 0, err
	}

	temporary := locate(fmt.Sprintf("%d.%s", time.Now().UnixNano(), uuid.Must(uuid.NewV4())))
	dest, err := os.Create(temporary)
	if err != nil {
		return "", 0, err
	}
	var iv [aes.BlockSize]byte

	stream := cipher.NewOFB(block, iv[:])
	encryptedWriter := &cipher.StreamWriter{S: stream, W: dest}
	// compress -> encrypt
	lz4Writer := lz4.NewWriter(encryptedWriter)
	size, err := io.Copy(lz4Writer, tee)
	if err != nil {
		dest.Close()
		os.Remove(temporary)
		return "", 0, err
	}
	// XXX: not to be trusted, attacker can flip bits
	// the only reason we encrypt is so we dont accidentally receive unencrypted data
	// or if someone steals the data

	lz4Writer.Close()
	encryptedWriter.Close()
	dest.Close()

	shasum := fmt.Sprintf("%x", sha.Sum(nil))
	err = os.Rename(temporary, locate(shasum))
	if err != nil {
		os.Remove(temporary)
		return "", 0, err
	}
	return shasum, size, nil
}

func decompressAndDecrypt(key string, r io.Reader) (io.Reader, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	var iv [aes.BlockSize]byte
	stream := cipher.NewOFB(block, iv[:])
	// compress -> encrypt -> decrypt -> decompress
	decryptReader := &cipher.StreamReader{S: stream, R: r}
	lz4Reader := lz4.NewReader(decryptReader)
	return lz4Reader, nil
	// XXX: not to be trusted, attacker can flip bits
	// the only reason we encrypt is so we dont accidentally receive unencrypted data
	// or if someone steals the data
}

func FindFile(db *gorm.DB, t *Token, p string) (*FileOrigin, error) {
	dir, name := split(p)
	fm := &FileMetadata{}
	if err := db.Where("user_id = ? AND token_id = ? AND filename = ? AND path = ?", t.UserID, t.ID, name, dir).Take(fm).Error; err != nil {
		return nil, err

	}
	fv := &FileVersion{}
	if err := db.Where("file_metadata_id = ?", fm.ID).Last(fv).Error; err != nil {
		return nil, err
	}

	fo := &FileOrigin{}
	if err := db.Where("id = ?", fv.FileOriginID).Take(fo).Error; err != nil {
		return nil, err
	}

	return fo, nil
}

func FindAndOpenFile(db *gorm.DB, t *Token, p string) (*FileOrigin, *os.File, io.Reader, error) {
	fo, err := FindFile(db, t, p)
	if err != nil {
		return nil, nil, nil, err
	}
	file, err := os.Open(fo.FSPath())
	if err != nil {
		return nil, nil, nil, err
	}

	reader, err := decompressAndDecrypt(t.Salt, file)
	if err != nil {
		file.Close()
		return nil, nil, nil, err
	}
	return fo, file, reader, nil

}
func ListVersionsFile(db *gorm.DB, t *Token, p string) ([]*FileVersion, error) {
	dir, name := split(p)
	fm := &FileMetadata{}
	if err := db.Where(FileMetadata{UserID: t.UserID, TokenID: t.ID, Path: dir, Filename: name}).Take(&fm).Error; err != nil {
		return nil, err
	}

	versions := []*FileVersion{}
	if err := db.Where("file_metadata_id = ?", fm.ID).Order("id").Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

func SaveFile(db *gorm.DB, t *Token, body io.Reader, p string) (*FileVersion, error) {
	dir, name := split(p)
	sha, size, err := saveUploadedFile(t.Salt, body)
	if err != nil {
		return nil, err
	}

	// create file origin
	fo := &FileOrigin{}
	tx := db.Begin()
	res := tx.Where("sha256 = ?", sha).Take(fo)
	fm := &FileMetadata{}
	if res.RecordNotFound() {
		// create new one
		fo.SHA256 = sha
		fo.Size = uint64(size)
		if err := tx.Save(fo).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	// create file metadata if we did not create it
	if err := tx.FirstOrCreate(&fm, FileMetadata{UserID: t.UserID, TokenID: t.ID, Path: dir, Filename: name}).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	// create the version
	fv := &FileVersion{}
	if err := tx.Where(FileVersion{FileMetadataID: fm.ID, FileOriginID: fo.ID}).FirstOrCreate(&fv).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// check how many versions we have of this file
	versions, err := ListVersionsFile(tx, t, p)
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	limit := int(t.NumberOfArchives + 1)
	removeFiles := []string{}
	if len(versions) > limit {
		toDelete := versions[0:(len(versions) - limit)]
		for _, rm := range toDelete {
			conflicts := 0
			if err := tx.Model(FileVersion{}).Where("file_origin_id = ? AND file_metadata_id != ?", rm.FileOriginID, rm.FileMetadataID).Count(&conflicts).Error; err != nil {
				tx.Rollback()
				return nil, err
			}

			if conflicts == 0 {
				toBeDeleted := &FileOrigin{}
				if err := tx.Where("ID = ?", rm.FileOriginID).Take(&toBeDeleted).Error; err != nil {
					tx.Rollback()
					return nil, err
				}

				if err := tx.Delete(toBeDeleted).Error; err != nil {
					tx.Rollback()
					return nil, err
				}

				removeFiles = append(removeFiles, toBeDeleted.FSPath())
			}
			if err := tx.Delete(rm).Error; err != nil {
				tx.Rollback()
				return nil, err
			}
		}
	}

	// goooo
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}

	for _, f := range removeFiles {
		log.Printf("removing %s, limit: %d, versions: %d", f, limit, len(versions))
		os.Remove(f)
	}

	return fv, nil
}