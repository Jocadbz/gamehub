package main

import (
	"database/sql"
	"encoding/base64"
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
	ID            int
	Name          string
	Description   string
	ImageData     string // Base64 encoded image data
	Screenshots   []string // Base64 encoded screenshot data
	DownloadPath  string // Path on server for download
}

type Config struct {
	Title   string `json:"title"`
	Port    int    `json:"port"`
	BaseURL string `json:"base_url"`
}

var db *sql.DB
var config Config

func main() {
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

	// Handle routes with normalized BaseURL
	// Use a root handler to catch exact BaseURL or BaseURL/
	rootPath := config.BaseURL
	if rootPath == "" {
		rootPath = "/"
	}
	http.HandleFunc(rootPath, homeHandler)
	// Also handle with trailing slash if BaseURL is not empty
	if config.BaseURL != "" {
		http.HandleFunc(config.BaseURL+"/", homeHandler)
	}
	http.HandleFunc(config.BaseURL+"/game/", gameHandler)
	http.HandleFunc(config.BaseURL+"/upload", uploadHandler)
	http.HandleFunc(config.BaseURL+"/upload-game", uploadGameHandler)
	http.HandleFunc(config.BaseURL+"/edit/", editHandler)
	http.HandleFunc(config.BaseURL+"/update-game/", updateGameHandler)
	http.HandleFunc(config.BaseURL+"/delete/", deleteHandler)
	http.HandleFunc(config.BaseURL+"/download/", downloadHandler)
	http.Handle(config.BaseURL+"/static/", http.StripPrefix(config.BaseURL+"/static/", http.FileServer(http.Dir("static"))))

	fmt.Printf("Server running on :%d with base URL %s\n", config.Port, config.BaseURL)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil))
}

func createTables() {
	query := `
	CREATE TABLE IF NOT EXISTS games (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		description TEXT,
		image_data BLOB,
		download_path TEXT
	);
	CREATE TABLE IF NOT EXISTS screenshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		game_id INTEGER,
		image_data BLOB,
		FOREIGN KEY (game_id) REFERENCES games(id)
	);`
	_, err := db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, name, image_data FROM games")
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var games []Game
	for rows.Next() {
		var g Game
		var imageData []byte
		if err := rows.Scan(&g.ID, &g.Name, &imageData); err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		if imageData != nil {
			g.ImageData = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageData)
		}
		games = append(games, g)
	}

	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, struct {
		Title   string
		Games   []Game
		BaseURL string
	}{
		Title:   config.Title,
		Games:   games,
		BaseURL: config.BaseURL,
	})
}

func gameHandler(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, config.BaseURL+"/game/")
	var game Game
	var imageData []byte
	err := db.QueryRow("SELECT id, name, description, image_data, download_path FROM games WHERE id = ?", idStr).Scan(&game.ID, &game.Name, &game.Description, &imageData, &game.DownloadPath)
	if err != nil {
		http.Error(w, "Game not found", http.StatusNotFound)
		return
	}
	if imageData != nil {
		game.ImageData = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageData)
	}

	rows, err := db.Query("SELECT image_data FROM screenshots WHERE game_id = ?", idStr)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var screenshotData []byte
		if err := rows.Scan(&screenshotData); err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		if screenshotData != nil {
			game.Screenshots = append(game.Screenshots, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(screenshotData))
		}
	}

	tmpl, err := template.ParseFiles("templates/game.html")
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, struct {
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
	tmpl, err := template.ParseFiles("templates/upload.html")
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, struct {
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

	result, err := db.Exec("INSERT INTO games (name, description, image_data, download_path) VALUES (?, ?, ?, ?)", name, description, imageData, downloadPath)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	gameID, _ := result.LastInsertId()
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("screenshot%d", i+1)
		screenshotFile, _, err := r.FormFile(key)
		if err != nil {
			continue
		}
		defer screenshotFile.Close()

		screenshotData, err := io.ReadAll(screenshotFile)
		if err != nil {
			continue
		}

		_, err = db.Exec("INSERT INTO screenshots (game_id, image_data) VALUES (?, ?)", gameID, screenshotData)
		if err != nil {
			continue
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

	tmpl, err := template.ParseFiles("templates/edit.html")
	if err != nil {
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, struct {
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
		_, err = db.Exec("UPDATE games SET name = ?, description = ?, image_data = ?, download_path = ? WHERE id = ?", name, description, imageData, downloadPath, idStr)
	} else {
		_, err = db.Exec("UPDATE games SET name = ?, description = ?, download_path = ? WHERE id = ?", name, description, downloadPath, idStr)
	}
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Handle screenshot updates if provided
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("screenshot%d", i+1)
		screenshotFile, _, err := r.FormFile(key)
		if err == nil {
			defer screenshotFile.Close()
			screenshotData, err := io.ReadAll(screenshotFile)
			if err == nil {
				_, err = db.Exec("INSERT OR REPLACE INTO screenshots (game_id, image_data) VALUES (?, ?)", idStr, screenshotData)
				if err != nil {
					continue
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
		http.Error(w, "Invalid download path", http.StatusBadRequest)
		return
	}

	// Get the original filename from the path
	filename := filepath.Base(decodedPath)
	// Set the Content-Disposition header to suggest the original filename
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	// Serve the file from the server path
	http.ServeFile(w, r, decodedPath)
}