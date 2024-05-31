package main

import (
	"encoding/base64"
	"fmt"
	"github.com/labstack/echo/v5"
	"github.com/pocketbase/pocketbase/forms"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/models/schema"
	"github.com/pocketbase/pocketbase/plugins/ghupdate"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func main() {
	app := pocketbase.New()

	// ---------------------------------------------------------------
	// Optional plugin flags:
	// ---------------------------------------------------------------

	var hooksDir string
	app.RootCmd.PersistentFlags().StringVar(
		&hooksDir,
		"hooksDir",
		"",
		"the directory with the JS app hooks",
	)

	var hooksWatch bool
	app.RootCmd.PersistentFlags().BoolVar(
		&hooksWatch,
		"hooksWatch",
		true,
		"auto restart the app on pb_hooks file change",
	)

	var hooksPool int
	app.RootCmd.PersistentFlags().IntVar(
		&hooksPool,
		"hooksPool",
		25,
		"the total prewarm goja.Runtime instances for the JS app hooks execution",
	)

	var migrationsDir string
	app.RootCmd.PersistentFlags().StringVar(
		&migrationsDir,
		"migrationsDir",
		"",
		"the directory with the user defined migrations",
	)

	var automigrate bool
	app.RootCmd.PersistentFlags().BoolVar(
		&automigrate,
		"automigrate",
		true,
		"enable/disable auto migrations",
	)

	var publicDir string
	app.RootCmd.PersistentFlags().StringVar(
		&publicDir,
		"publicDir",
		defaultPublicDir(),
		"the directory to serve static files",
	)

	var indexFallback bool
	app.RootCmd.PersistentFlags().BoolVar(
		&indexFallback,
		"indexFallback",
		true,
		"fallback the request to index.html on missing static path (eg. when pretty urls are used with SPA)",
	)

	var queryTimeout int
	app.RootCmd.PersistentFlags().IntVar(
		&queryTimeout,
		"queryTimeout",
		30,
		"the default SELECT queries timeout in seconds",
	)

	app.RootCmd.ParseFlags(os.Args[1:])

	// ---------------------------------------------------------------
	// Plugins and hooks:
	// ---------------------------------------------------------------

	// load jsvm (hooks and migrations)
	jsvm.MustRegister(app, jsvm.Config{
		MigrationsDir: migrationsDir,
		HooksDir:      hooksDir,
		HooksWatch:    hooksWatch,
		HooksPoolSize: hooksPool,
	})

	// migrate command (with js templates)
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		TemplateLang: migratecmd.TemplateLangJS,
		Automigrate:  automigrate,
		Dir:          migrationsDir,
	})

	// GitHub selfupdate
	ghupdate.MustRegister(app, app.RootCmd, ghupdate.Config{})

	app.OnAfterBootstrap().PreAdd(func(e *core.BootstrapEvent) error {
		app.Dao().ModelQueryTimeout = time.Duration(queryTimeout) * time.Second
		return nil
	})

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
		var uploadsCollectionId string

		existingUploadsCollection, err := app.Dao().FindCollectionByNameOrId("uploads")
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
					MinSelect:     nil,
					MaxSelect:     types.Pointer(1),
					CollectionId:  "_pb_users_auth_",
					CascadeDelete: true,
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

			existingUploadsCollection, err = app.Dao().FindCollectionByNameOrId("uploads")
			if err != nil {
				panic(fmt.Errorf("uploads collection not found even after creating it"))
			}
		}
		uploadsCollectionId = existingUploadsCollection.Id

		app.Logger().Debug("", "uploadsCollectionId", uploadsCollectionId)

		_, err = app.Dao().FindCollectionByNameOrId("accessRefs")
		if err != nil {
			collection := &models.Collection{}

			form := forms.NewCollectionUpsert(app, collection)
			form.Name = "accessRefs"
			form.Type = models.CollectionTypeBase
			form.ListRule = types.Pointer("user.id = @request.auth.id")
			form.ViewRule = types.Pointer("user.id = @request.auth.id")
			form.CreateRule = types.Pointer("user.id = @request.auth.id && upload.user.id = @request.auth.id")
			form.UpdateRule = types.Pointer("user.id = @request.auth.id")
			form.DeleteRule = types.Pointer("user.id = @request.auth.id")
			form.Schema.AddField(&schema.SchemaField{
				Name:     "upload",
				Type:     schema.FieldTypeRelation,
				Required: true,
				Options: &schema.RelationOptions{
					MinSelect:     nil,
					MaxSelect:     types.Pointer(1),
					CollectionId:  uploadsCollectionId,
					CascadeDelete: true,
				},
			})
			form.Schema.AddField(&schema.SchemaField{
				Name:     "user",
				Type:     schema.FieldTypeRelation,
				Required: true,
				Options: &schema.RelationOptions{
					MinSelect:     nil,
					MaxSelect:     types.Pointer(1),
					CollectionId:  "_pb_users_auth_",
					CascadeDelete: true,
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

			// TODO: use host header for app url, otherwise it's bound for only one url with cors restrictions
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

		e.Router.GET("/accref/:access_ref_id", func(c echo.Context) error {
			var accessRefId string

			if accessRefId = c.PathParam("access_ref_id"); accessRefId == "" {
				return apis.NewBadRequestError("no access_ref_id", nil)
			}

			accessRef, err := app.Dao().FindRecordById("accessRefs", accessRefId)
			if err != nil {
				return apis.NewNotFoundError("", nil)
			}

			if errs := app.Dao().ExpandRecord(accessRef, []string{"upload"}, nil); len(errs) > 0 {
				return apis.NewApiError(http.StatusInternalServerError, fmt.Sprintf("failed to expand: %v", errs), nil)
			}

			upload := accessRef.ExpandedOne("upload")
			if upload == nil {
				return apis.NewNotFoundError("", nil)
			}

			key := upload.BaseFilesPath() + "/" + upload.GetString("file")

			fsys, _ := app.NewFilesystem()
			defer fsys.Close()

			blob, _ := fsys.GetFile(key)
			defer blob.Close()

			c.Response().Header().Set("Content-Type", upload.GetString("mime_type"))

			http.ServeContent(c.Response(), c.Request(), upload.GetString("filename"), upload.Updated.Time(), blob)

			//c.Response().Header().Set("Accept-Ranges", "bytes")
			//c.Response().Header().Set("Content-Disposition", "attachment; filename=\""+upload.GetString("filename")+"\"")
			//
			//return c.Stream(200, upload.GetString("mime_type"), blob)

			return nil
		})

		e.Router.GET("/*", apis.StaticDirectoryHandler(os.DirFS(publicDir), indexFallback))

		return nil
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// the default pb_public dir location is relative to the executable
func defaultPublicDir() string {
	if strings.HasPrefix(os.Args[0], os.TempDir()) {
		// most likely ran with go run
		return "./pb_public"
	}

	return filepath.Join(os.Args[0], "../pb_public")
}
