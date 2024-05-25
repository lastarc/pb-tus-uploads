package main

import (
	"encoding/base64"
	"fmt"
	"github.com/labstack/echo/v5"
	"github.com/pocketbase/pocketbase/forms"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/models/schema"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func main() {
	app := pocketbase.New()

	uploadAndCleanup := func(uploadId string) error {
		upload, err := app.Dao().FindRecordById("uploads", uploadId)
		if err != nil {
			return err
		}

		if upload.GetInt("size") == 0 || upload.GetInt("size") != upload.GetInt("current_offset") {
			return fmt.Errorf("upload not finished")
		}

		// check if local file exists
		_, err = os.Stat(filepath.Join(app.DataDir(), "tus_uploads", upload.Id+".part"))
		if err != nil && os.IsNotExist(err) {
			return nil // nothing we can do
		} else if err != nil {
			return err
		}

		form := forms.NewRecordUpsert(app, upload)

		file, err := filesystem.NewFileFromPath(filepath.Join(app.DataDir(), "tus_uploads", upload.Id+".part"))
		if err != nil {
			return err
		}
		file.Name = upload.GetString("filename")

		err = form.AddFiles("file", file)
		if err != nil {
			return err
		}

		if err = form.Submit(); err != nil {
			return err
		}

		err = os.Remove(filepath.Join(app.DataDir(), "tus_uploads", upload.Id+".part"))
		if err != nil {
			return err
		}

		app.Logger().Debug("uploaded and cleaned up", "uploadId", upload.Id)

		return nil
	}

	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {

		_, err := app.Dao().FindCollectionByNameOrId("uploads")
		if err != nil {
			collection := &models.Collection{}

			form := forms.NewCollectionUpsert(app, collection)
			form.Name = "uploads"
			form.Type = models.CollectionTypeBase
			form.ListRule = types.Pointer("user.id = @request.auth.id")
			form.ViewRule = types.Pointer("user.id = @request.auth.id")
			form.CreateRule = nil
			form.UpdateRule = nil
			form.DeleteRule = nil
			form.Schema.AddField(&schema.SchemaField{
				Name: "current_offset",
				Type: schema.FieldTypeNumber,
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "size",
				Type:     schema.FieldTypeNumber,
				Required: true,
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "filename",
				Type:     schema.FieldTypeText,
				Required: true,
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "mime_type",
				Type:     schema.FieldTypeText,
				Required: true,
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "user",
				Type:     schema.FieldTypeRelation,
				Required: true,
				Options: &schema.RelationOptions{
					MinSelect:    nil,
					MaxSelect:    types.Pointer(1),
					CollectionId: "_pb_users_auth_",
				},
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "file",
				Type:     schema.FieldTypeFile,
				Required: false,
				Options: &schema.FileOptions{
					MaxSelect: 1,
					MaxSize:   4 * 1024 * 1024 * 1024, // 4GB
					Protected: true,
				},
			})

			if err := form.Submit(); err != nil {
				return err
			}
		}

		// check stray finished uploads
		uploads, err := app.Dao().FindRecordsByFilter(
			"uploads",
			"size != 0 && size = current_offset",
			"-updated",
			0,
			0,
		)
		if err != nil {
			return err
		}

		for _, upload := range uploads {
			err = uploadAndCleanup(upload.Id)
			if err != nil {
				app.Logger().Error("upload failed", "uploadId", upload.Id)
			}
		}

		e.Router.POST("/uploads", func(c echo.Context) error {
			c.Response().Header().Set("Tus-Resumable", "1.0.0")

			headers := apis.RequestInfo(c).Headers
			authRecord := apis.RequestInfo(c).AuthRecord

			if v, ok := headers["tus_resumable"]; !ok || v != "1.0.0" {
				c.Response().Header().Set("Tus-Version", "1.0.0")
				return apis.NewApiError(http.StatusPreconditionFailed, "", nil)
			}

			var (
				err      error
				metadata = map[string]string{}

				size     = 0
				filename = ""
				mimeType = ""
			)

			// headers are lowercased with underscore (_) instead of dashes (-)

			if size, err = strconv.Atoi(c.Request().Header.Get("Upload-Length")); err != nil {
				return apis.NewBadRequestError("", nil)
			}

			if h := c.Request().Header.Get("Upload-Metadata"); h != "" {
				for _, str := range strings.Split(h, ",") {
					items := strings.Split(strings.TrimSpace(str), " ")
					if len(items) != 2 {
						continue
					}

					var valBytes []byte

					valBytes, err = base64.StdEncoding.DecodeString(items[1])
					if err != nil {
						continue
					}

					metadata[items[0]] = string(valBytes)
				}
			}

			var ok bool

			if filename, ok = metadata["filename"]; !ok {
				c.Response().Header().Set("Tus-Version", "1.0.0")
				return apis.NewBadRequestError("", nil)
			}

			if mimeType, ok = metadata["filetype"]; !ok {
				c.Response().Header().Set("Tus-Version", "1.0.0")
				return apis.NewBadRequestError("", nil)
			}

			collection, err := app.Dao().FindCollectionByNameOrId("uploads")
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}

			record := models.NewRecord(collection)

			form := forms.NewRecordUpsert(app, record)

			err = form.LoadData(map[string]any{
				"current_offset": 0,
				"size":           size,
				"filename":       filename,
				"mime_type":      mimeType,
				"user":           authRecord.Id,
			})
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "failed to load form data", err)
			}

			if err = form.Submit(); err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "failed to create new upload record", err)
			}

			location := fmt.Sprintf("%s/uploads/%s", app.Settings().Meta.AppUrl, record.Id)
			c.Response().Header().Set("Location", location)
			return c.NoContent(http.StatusCreated)
		}, apis.RequireRecordAuth())

		e.Router.HEAD("/uploads/:upload_id", func(c echo.Context) error {
			c.Response().Header().Set("Tus-Resumable", "1.0.0")

			headers := apis.RequestInfo(c).Headers

			if v, ok := headers["tus_resumable"]; !ok || v != "1.0.0" {
				c.Response().Header().Set("Tus-Version", "1.0.0")
				return apis.NewApiError(http.StatusPreconditionFailed, "", nil)
			}

			var uploadId string

			if uploadId = c.PathParam("upload_id"); uploadId == "" {
				return apis.NewBadRequestError("", nil)
			}

			upload, err := app.Dao().FindRecordById("uploads", uploadId)
			if err != nil {
				return apis.NewNotFoundError("", nil)
			}

			c.Response().Header().Set("Cache-Control", "no-store")

			c.Response().Header().Set("Upload-Offset", upload.GetString("current_offset"))
			c.Response().Header().Set("Upload-Length", upload.GetString("size"))

			return c.NoContent(http.StatusOK)
		}, apis.RequireRecordAuth())

		e.Router.PATCH("/uploads/:upload_id", func(c echo.Context) error {
			c.Response().Header().Set("Tus-Resumable", "1.0.0")

			headers := apis.RequestInfo(c).Headers

			if v, ok := headers["tus_resumable"]; !ok || v != "1.0.0" {
				c.Response().Header().Set("Tus-Version", "1.0.0")
				return apis.NewApiError(http.StatusPreconditionFailed, "", nil)
			}

			if v, ok := headers["content_type"]; !ok || v != "application/offset+octet-stream" {
				return apis.NewApiError(http.StatusUnsupportedMediaType, "", nil)
			}

			var (
				err error

				contentLength int
				uploadOffset  int
				uploadId      string
			)

			if contentLength, err = strconv.Atoi(c.Request().Header.Get("Content-Length")); err != nil {
				return apis.NewBadRequestError("no Content-Length", nil)
			}

			if uploadOffset, err = strconv.Atoi(c.Request().Header.Get("Upload-Offset")); err != nil {
				return apis.NewBadRequestError("no Upload-Offset", nil)
			}

			if uploadId = c.PathParam("upload_id"); uploadId == "" {
				return apis.NewBadRequestError("no upload_id", nil)
			}

			upload, err := app.Dao().FindRecordById("uploads", uploadId)
			if err != nil {
				return apis.NewNotFoundError("", nil)
			}

			if uploadOffset != upload.GetInt("current_offset") || uploadOffset >= upload.GetInt("size") {
				return apis.NewApiError(http.StatusConflict, "", nil)
			}

			err = os.MkdirAll(filepath.Join(app.DataDir(), "tus_uploads"), 0750)
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}

			f, err := os.OpenFile(filepath.Join(app.DataDir(), "tus_uploads", upload.Id+".part"), os.O_RDWR|os.O_CREATE, 0755)
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}
			defer f.Close()

			_, err = f.Seek(int64(uploadOffset), 0)
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}

			body, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}
			defer c.Request().Body.Close()

			written, err := f.Write(body)
			if err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}

			if written != contentLength {
				return apis.NewApiError(http.StatusInternalServerError, "written != contentLength", nil)
			}

			upload.Set("current_offset", strconv.Itoa(uploadOffset+contentLength))

			if err = app.Dao().SaveRecord(upload); err != nil {
				return apis.NewApiError(http.StatusInternalServerError, "", nil)
			}

			if upload.GetInt("size") != 0 && upload.GetInt("size") == upload.GetInt("current_offset") {
				go uploadAndCleanup(upload.Id)
			}

			c.Response().Header().Set("Upload-Offset", upload.GetString("current_offset"))
			c.Response().Header().Set("Upload-Length", upload.GetString("size"))

			return c.NoContent(http.StatusOK)
		}, apis.RequireRecordAuth())

		e.Router.GET("/*", apis.StaticDirectoryHandler(os.DirFS("./pb_public"), false))

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
