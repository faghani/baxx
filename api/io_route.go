package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackdoe/baxx/common"
	"github.com/jackdoe/baxx/file"

	"github.com/jinzhu/gorm"
)

func FileLine(fm *file.FileMetadata, fv *file.FileVersion) string {
	isCurrent := ""
	if fm.LastVersionID == fv.ID {
		isCurrent = "*"
	}
	return fmt.Sprintf("%d\t%s\t%s@v%d%s\t%s\n", fv.Size, fv.CreatedAt.Format(time.ANSIC), fm.FullPath(), fv.ID, isCurrent, fv.SHA256)
}

func SaveFileProcess(s *file.Store, db *gorm.DB, t *file.Token, body io.Reader, p string) (*file.FileVersion, *file.FileMetadata, error) {
	leftSize, leftInodes, err := file.GetQuotaLeft(db, t)
	if err != nil {
		return nil, nil, err
	}

	if leftSize < 0 {
		return nil, nil, errors.New("quota limit reached")
	}

	if leftInodes < 1 {
		return nil, nil, errors.New("inode quota limit reached")
	}

	return file.SaveFile(s, db, t, p, body)
}

func LSAL(files []file.FileMetadataAndVersion) string {
	buf := bytes.NewBufferString("")
	grouped := map[string][]file.FileMetadataAndVersion{}

	for _, f := range files {
		grouped[f.FileMetadata.Path] = append(grouped[f.FileMetadata.Path], f)
	}

	keys := []string{}
	for p := range grouped {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	total := uint64(0)
	for _, k := range keys {
		files := grouped[k]

		size := uint64(0)
		for _, f := range files {
			for _, v := range f.Versions {
				size += v.Size
				total += v.Size
			}
		}
		fmt.Fprintf(buf, "%s: total size: %d (%s)\n", k, size, common.PrettySize(size))
		for _, f := range files {
			for _, v := range f.Versions {
				buf.WriteString(FileLine(f.FileMetadata, v))
			}
		}
		fmt.Fprintf(buf, "\n")
	}
	fmt.Fprintf(buf, "sum total size: %d (%s)\n", total, common.PrettySize(total))
	return buf.String()
}

func setupIO(srv *server) {
	r := srv.r
	store := srv.store
	db := srv.db
	getViewTokenLoggedOrNot := srv.getViewTokenLoggedOrNot
	download := func(c *gin.Context) {
		t, _, err := getViewTokenLoggedOrNot(c)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		fv, _, err := file.FindFile(db, t, c.Param("path"))
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return

		}

		reader, err := store.DownloadFile(t.Salt, t.Bucket, fv.StoreID)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// FIXME: close?
		c.Header("Content-Length", fmt.Sprintf("%d", fv.Size))

		c.Header("Content-Disposition", "attachment; filename="+fv.SHA256+".sha") // make sure people dont use it for loading js
		c.Header("Content-Transfer-Encoding", "binary")
		c.Header("Content-Type", "application/octet-stream")
		c.DataFromReader(http.StatusOK, int64(fv.Size), "octet/stream", reader, map[string]string{})
	}

	upload := func(c *gin.Context) {
		body := c.Request.Body
		defer body.Close()

		t, _, err := getViewTokenLoggedOrNot(c)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		p := c.Param("path")
		fv, fm, err := SaveFileProcess(store, db, t, body, p)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// check if over quota

		actionLog(db, t.UserID, "file", "upload", c.Request, fmt.Sprintf("FileVersion: %d/%d", fv.ID, fv.FileMetadataID))
		if wantJson(c) {
			c.IndentedJSON(http.StatusOK, fv)
			return
		}
		c.String(http.StatusOK, FileLine(fm, fv))
	}

	deleteFile := func(c *gin.Context) {
		t, _, err := getViewTokenLoggedOrNot(c)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		force := false
		recursive := false
		var json common.Force
		if err := c.ShouldBindJSON(&json); err == nil {
			if json.Force != nil {
				force = *json.Force
			}

			if json.Recursive != nil {
				recursive = *json.Recursive
			}
		}
		p := c.Param("path")
		n := 0

		if force {
			if err := file.DeleteFileWithPath(store, db, t, p); err == nil {
				n++
			}
			files, err := file.ListFilesInPath(db, t, p, !recursive)
			if err == nil {
				for _, f := range files {
					if err := file.DeleteFile(store, db, t, f.FileMetadata); err == nil {
						n++
					}
				}
			}
		} else {
			if err := file.DeleteFileWithPath(store, db, t, p); err != nil {
				warnErr(c, err)
				c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			n = 1
		}

		actionLog(db, t.UserID, "file", "delete", c.Request, "")
		c.IndentedJSON(http.StatusOK, &common.DeleteSuccess{Success: true, Count: n})
	}

	listFiles := func(c *gin.Context) {
		t, _, err := getViewTokenLoggedOrNot(c)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		p := c.Param("path")
		if !strings.HasSuffix(p, "/") {
			p = p + "/"
		}

		files, err := file.ListFilesInPath(db, t, p, false)
		if err != nil {
			warnErr(c, err)
			c.IndentedJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if wantJson(c) {
			c.IndentedJSON(http.StatusOK, files)
			return
		}
		c.String(http.StatusOK, LSAL(files))
	}

	mutateSinglePATH := "/io/:token/*path"
	r.GET(mutateSinglePATH, download)
	r.POST(mutateSinglePATH, upload)
	r.PUT(mutateSinglePATH, upload)
	r.DELETE(mutateSinglePATH, deleteFile)

	for _, a := range []string{"dir", "ls"} {
		lsPath := "/" + a + "/:token/*path"
		r.GET(lsPath, listFiles)
	}

}
