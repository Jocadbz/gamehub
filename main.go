package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	_ "github.com/mattn/go-sqlite3"
)

type Game struct {
	ID           int
	Name         string
	Description  string
	ImageData    string // Image path
	ImagePath    string
	Screenshots  []string // Base64 encoded screenshot data
	DownloadPath string   // Path on server for download
	CreatedAt    string   // Timestamp when added
}

type Config struct {
	Title     string `json:"title"`
	Port      int    `json:"port"`
	BaseURL   string `json:"base_url"`
	AdminUser string `json:"admin_user"`
	AdminPass string `json:"admin_password"`
}

var db *sql.DB
var config Config
var templates *template.Template

func main() {
	// Parse templates once
	templates = template.Must(template.ParseGlob("templates/*.html"))

	// Load configuration
	configFile, err := os.Open("./config.json")
	if err != nil {
		log.Fatal("Error opening config file:", err)
	}
	defer configFile.Close()

	decoder := json.NewDecoder(configFile)
	err = decoder.Decode(&config)
	if err != nil {
		log.Fatal("Error decoding config file:", err)
	}

	// Normalize BaseURL by removing leading and trailing slashes for consistency
	config.BaseURL = strings.Trim(config.BaseURL, "/")
	if config.BaseURL != "" {
		config.BaseURL = "/" + config.BaseURL
	} else {
		config.BaseURL = ""
	}

	db, err = sql.Open("sqlite3", "./gamehub.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	createTables()
	migrateDB()
	migrateImages()

	// Handle routes with normalized BaseURL
	rootPath := config.BaseURL
	if rootPath == "" {
		rootPath = "/"
	}
	http.HandleFunc(rootPath, homeHandler)
	if config.BaseURL != "" {
		http.HandleFunc(config.BaseURL+"/", homeHandler)
	}
	http.HandleFunc(config.BaseURL+"/game/", gameHandler)
	http.HandleFunc(config.BaseURL+"/upload", basicAuth(uploadHandler))
	http.HandleFunc(config.BaseURL+"/upload-game", basicAuth(uploadGameHandler))
	http.HandleFunc(config.BaseURL+"/edit/", basicAuth(editHandler))
	http.HandleFunc(config.BaseURL+"/update-game/", basicAuth(updateGameHandler))
	http.HandleFunc(config.BaseURL+"/delete/", basicAuth(deleteHandler))
	http.HandleFunc(config.BaseURL+"/download/", downloadHandler)
	http.Handle(config.BaseURL+"/static/", http.StripPrefix(config.BaseURL+"/static/", http.FileServer(http.Dir("static"))))
	http.Handle(config.BaseURL+"/uploads/", http.StripPrefix(config.BaseURL+"/uploads/", http.FileServer(http.Dir("uploads"))))

	fmt.Printf("Server running on :%d with base URL %s\n", config.Port, config.BaseURL)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if config.AdminUser == "" || config.AdminPass == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != config.AdminUser || pass != config.AdminPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func createTables() {
	query := `
	CREATE TABLE IF NOT EXISTS games (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		description TEXT,
		image_data BLOB,
		image_path TEXT,
		download_path TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS screenshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_id INTEGER,
		image_data BLOB,
		image_path TEXT,
		FOREIGN KEY (game_id) REFERENCES games(id)
	);`
	_, err := db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}
}

// migrateDB adds new columns to existing databases that lack them.

func migrateImages() {
	os.MkdirAll("uploads/images", 0755)

	// Migrate games
	type imgData struct {
		id   int
		data []byte
	}
	var gameImages []imgData
	rows, err := db.Query("SELECT id, image_data FROM games WHERE image_data IS NOT NULL")
	if err == nil {
		for rows.Next() {
			var id int
			var data []byte
			if err := rows.Scan(&id, &data); err == nil && len(data) > 0 {
				gameImages = append(gameImages, imgData{id, data})
			}
		}
		rows.Close()
	}

	for _, g := range gameImages {
		path := fmt.Sprintf("uploads/images/game_%d.jpg", g.id)
		if err := os.WriteFile(path, g.data, 0644); err == nil {
			_, err = db.Exec("UPDATE games SET image_path = ?, image_data = NULL WHERE id = ?", "/"+path, g.id)
			if err != nil {
				log.Printf("Failed to update image_path for game %d: %v", g.id, err)
			}
		} else {
			log.Printf("Failed to write image for game %d: %v", g.id, err)
		}
	}

	// Migrate screenshots
	var screenImages []imgData
	sRows, err := db.Query("SELECT id, image_data FROM screenshots WHERE image_data IS NOT NULL")
	if err == nil {
		for sRows.Next() {
			var id int
			var data []byte
			if err := sRows.Scan(&id, &data); err == nil && len(data) > 0 {
				screenImages = append(screenImages, imgData{id, data})
			}
		}
		sRows.Close()
	}

	for _, s := range screenImages {
		path := fmt.Sprintf("uploads/images/screenshot_%d.jpg", s.id)
		if err := os.WriteFile(path, s.data, 0644); err == nil {
			_, err = db.Exec("UPDATE screenshots SET image_path = ?, image_data = NULL WHERE id = ?", "/"+path, s.id)
			if err != nil {
				log.Printf("Failed to update image_path for screenshot %d: %v", s.id, err)
			}
		} else {
			log.Printf("Failed to write image for screenshot %d: %v", s.id, err)
		}
	}
}

func migrateDB() {
	// Check if created_at column exists
	rows, err := db.Query("PRAGMA table_info(games)")
	if err != nil {
		log.Fatal(err)
	}

	hasCreatedAt := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			log.Fatal(err)
		}
		if name == "created_at" {
			hasCreatedAt = true
		}
	}
	rows.Close()

	if !hasCreatedAt {
		// SQLite does not allow ALTER TABLE ADD COLUMN with a non-constant
		// default like CURRENT_TIMESTAMP, so we add with no default and
		// backfill existing rows immediately.
		_, err := db.Exec("ALTER TABLE games ADD COLUMN created_at DATETIME")
		if err != nil {
			log.Fatal("Error adding created_at column:", err)
		}
		// Backfill existing rows with current timestamp
		_, err = db.Exec("UPDATE games SET created_at = CURRENT_TIMESTAMP WHERE created_at IS NULL")
		if err != nil {
			log.Fatal("Error backfilling created_at:", err)
		}
		log.Println("Migration: added created_at column to games table")
	}

	// Ensure image_path exists on games
	var hasGamesImagePath bool
	rowsGames, err := db.Query("PRAGMA table_info(games)")
	if err == nil {
		for rowsGames.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dfltValue sql.NullString
			var pk int
			if err := rowsGames.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
				if name == "image_path" {
					hasGamesImagePath = true
				}
			}
		}
		rowsGames.Close()
	}

	if !hasGamesImagePath {
		_, err := db.Exec("ALTER TABLE games ADD COLUMN image_path TEXT")
		if err != nil {
			log.Printf("Error adding image_path to games: %v", err)
		} else {
			log.Println("Migration: added image_path to games")
		}
	}

	// Ensure image_path exists on screenshots
	var hasScreenshotsImagePath bool
	rowsScreenshots, err := db.Query("PRAGMA table_info(screenshots)")
	if err == nil {
		for rowsScreenshots.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dfltValue sql.NullString
			var pk int
			if err := rowsScreenshots.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
				if name == "image_path" {
					hasScreenshotsImagePath = true
				}
			}
		}
		rowsScreenshots.Close()
	}

	if !hasScreenshotsImagePath {
		_, err := db.Exec("ALTER TABLE screenshots ADD COLUMN image_path TEXT")
		if err != nil {
			log.Printf("Error adding image_path to screenshots: %v", err)
		} else {
			log.Println("Migration: added image_path to screenshots")
		}
	}

}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	// Parse query parameters for sorting
	sortBy := r.URL.Query().Get("sort")
	order := r.URL.Query().Get("order")

	// Default sort: newest first
	if sortBy == "" {
		sortBy = "date"
	}
	if order == "" {
		order = "desc"
	}

	// Build ORDER BY clause
	var orderClause string
	switch sortBy {
	case "name":
		if order == "asc" {
			orderClause = "ORDER BY name COLLATE NOCASE ASC"
		} else {
			orderClause = "ORDER BY name COLLATE NOCASE DESC"
		}
	case "date":
		if order == "asc" {
			orderClause = "ORDER BY created_at ASC"
		} else {
			orderClause = "ORDER BY created_at DESC"
		}
	default:
		orderClause = "ORDER BY created_at DESC"
	}

	query := fmt.Sprintf("SELECT id, name, image_path, created_at FROM games %s", orderClause)
	rows, err := db.QueryContext(r.Context(), query)
	if err != nil {
		log.Printf("homeHandler DB Query error: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var games []Game
	for rows.Next() {
		var g Game
		var imagePath sql.NullString
		var createdAt sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &imagePath, &createdAt); err != nil {
			log.Printf("homeHandler rows.Scan error: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		if imagePath.Valid && imagePath.String != "" {
			g.ImageData = config.BaseURL + imagePath.String
		} else {
			g.ImageData = ""
		}
		if createdAt.Valid {
			g.CreatedAt = createdAt.String
		}
		games = append(games, g)
	}

	templates.ExecuteTemplate(w, "index.html", struct {
		Title   string
		Games   []Game
		BaseURL string
		Sort    string
		Order   string
	}{
		Title:   config.Title,
		Games:   games,
		BaseURL: config.BaseURL,
		Sort:    sortBy,
		Order:   order,
	})
}

func gameHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/game/")
	var game Game
	var imagePath sql.NullString
	var createdAt sql.NullString
	err := db.QueryRowContext(r.Context(), "SELECT id, name, description, image_path, download_path, created_at FROM games WHERE id = ?", idStr).Scan(&game.ID, &game.Name, &game.Description, &imagePath, &game.DownloadPath, &createdAt)
	if err != nil {
		log.Printf("gameHandler QueryRow error: %v", err)
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}
	if imagePath.Valid && imagePath.String != "" {
		game.ImageData = config.BaseURL + imagePath.String
	}
	if createdAt.Valid {
		game.CreatedAt = createdAt.String
	}

	rows, err := db.Query("SELECT image_path FROM screenshots WHERE game_id = ?", idStr)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var sp sql.NullString
		if err := rows.Scan(&sp); err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		if sp.Valid && sp.String != "" {
			game.Screenshots = append(game.Screenshots, config.BaseURL+sp.String)
		}
	}

	templates.ExecuteTemplate(w, "game.html", struct {
		Title   string
		Game    Game
		BaseURL string
	}{
		Title:   config.Title,
		Game:    game,
		BaseURL: config.BaseURL,
	})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "upload.html", struct {
		Title   string
		BaseURL string
	}{
		Title:   config.Title,
		BaseURL: config.BaseURL,
	})
}

func uploadGameHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(10 << 20) // 10MB limit for images
	if err != nil {
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	description := r.FormValue("description")
	downloadPath := r.FormValue("download_path")

	// Handle main image upload
	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Unable to get image", http.StatusBadRequest)
		return
	}
	defer file.Close()

	imageData, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Unable to read image", http.StatusBadRequest)
		return
	}

	result, err := db.Exec("INSERT INTO games (name, description, download_path, created_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)", name, description, downloadPath)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	gameID, _ := result.LastInsertId()
	imagePath := fmt.Sprintf("/uploads/images/game_%d.jpg", gameID)
	os.WriteFile("."+imagePath, imageData, 0644)
	db.Exec("UPDATE games SET image_path = ? WHERE id = ?", imagePath, gameID)

	// Handle multiple screenshots
	screenshotFiles, ok := r.MultipartForm.File["screenshots"]
	if ok {
		for _, screenshotFileHeader := range screenshotFiles {
			if len(screenshotFileHeader.Filename) == 0 {
				continue
			}
			screenshotFile, err := screenshotFileHeader.Open()
			if err != nil {
				continue
			}
			defer screenshotFile.Close()

			screenshotData, err := io.ReadAll(screenshotFile)
			if err != nil {
				continue
			}

			res, err := db.Exec("INSERT INTO screenshots (game_id) VALUES (?)", gameID)
			if err == nil {
				scID, _ := res.LastInsertId()
				scPath := fmt.Sprintf("/uploads/images/screenshot_%d.jpg", scID)
				os.WriteFile("."+scPath, screenshotData, 0644)
				db.Exec("UPDATE screenshots SET image_path = ? WHERE id = ?", scPath, scID)
			}

			if err != nil {
				continue
			}
		}
	}

	http.Redirect(w, r, config.BaseURL, http.StatusSeeOther)
}

func editHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/edit/")
	var game Game
	err := db.QueryRow("SELECT id, name, description, download_path FROM games WHERE id = ?", idStr).Scan(&game.ID, &game.Name, &game.Description, &game.DownloadPath)
	if err != nil {
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}

	templates.ExecuteTemplate(w, "edit.html", struct {
		Title   string
		Game    Game
		BaseURL string
	}{
		Title:   config.Title,
		Game:    game,
		BaseURL: config.BaseURL,
	})
}

func updateGameHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/update-game/")
	err := r.ParseMultipartForm(10 << 20) // 10MB limit for images
	if err != nil {
		http.Error(w, "Unable to parse form", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	description := r.FormValue("description")
	downloadPath := r.FormValue("download_path")

	// Handle main image upload if provided
	var imageData []byte
	file, _, err := r.FormFile("image")
	if err == nil {
		defer file.Close()
		imageData, err = io.ReadAll(file)
		if err != nil {
			http.Error(w, "Unable to read image", http.StatusBadRequest)
			return
		}
	}

	// Update game details
	if len(imageData) > 0 {
		imagePath := fmt.Sprintf("/uploads/images/game_%s.jpg", idStr)
		os.WriteFile("."+imagePath, imageData, 0644)
		_, err = db.Exec("UPDATE games SET name = ?, description = ?, image_path = ?, download_path = ? WHERE id = ?", name, description, imagePath, downloadPath, idStr)
	} else {
		_, err = db.Exec("UPDATE games SET name = ?, description = ?, download_path = ? WHERE id = ?", name, description, downloadPath, idStr)
	}
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Handle screenshot updates if provided - clear existing and add new ones
	if r.MultipartForm != nil {
		screenshotFiles, ok := r.MultipartForm.File["screenshots"]
		if ok && len(screenshotFiles) > 0 {
			// Delete existing screenshots for this game
			_, err = db.Exec("DELETE FROM screenshots WHERE game_id = ?", idStr)
			if err != nil {
				log.Printf("Error deleting old screenshots: %v", err)
			}

			// Add new screenshots
			for _, screenshotFileHeader := range screenshotFiles {
				if len(screenshotFileHeader.Filename) == 0 {
					continue
				}
				screenshotFile, err := screenshotFileHeader.Open()
				if err == nil {
					defer screenshotFile.Close()
					screenshotData, err := io.ReadAll(screenshotFile)
					if err == nil {
						res, err := db.Exec("INSERT INTO screenshots (game_id) VALUES (?)", idStr)
						if err == nil {
							scID, _ := res.LastInsertId()
							scPath := fmt.Sprintf("/uploads/images/screenshot_%d.jpg", scID)
							os.WriteFile("."+scPath, screenshotData, 0644)
							db.Exec("UPDATE screenshots SET image_path = ? WHERE id = ?", scPath, scID)
						}
					}
				}
			}
		}
	}

	http.Redirect(w, r, config.BaseURL+"/game/"+idStr, http.StatusSeeOther)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/delete/")
	// Delete screenshots first due to foreign key constraint
	_, err := db.Exec("DELETE FROM screenshots WHERE game_id = ?", idStr)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Delete game entry (does not touch the actual file on server)
	_, err = db.Exec("DELETE FROM games WHERE id = ?", idStr)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, config.BaseURL, http.StatusSeeOther)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/download/")
	var downloadPath string
	var gameName string
	err := db.QueryRow("SELECT download_path, name FROM games WHERE id = ?", idStr).Scan(&downloadPath, &gameName)
	if err != nil {
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}

	// Decode the path to handle spaces and special characters
	decodedPath, err := url.QueryUnescape(downloadPath)
	if err != nil {
		log.Printf("downloadHandler QueryUnescape error: %v", err)
		http.Error(w, "Invalid download path", http.StatusBadRequest)
		return
	}

	cleanPath := filepath.Clean(decodedPath)
	if strings.Contains(cleanPath, "..") {
		log.Printf("downloadHandler path traversal attempt blocked: %s", cleanPath)
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	// Get the original filename from the path
	filename := filepath.Base(decodedPath)
	// Set the Content-Disposition header to suggest the original filename
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	// Serve the file from the server path
	http.ServeFile(w, r, decodedPath)
}
