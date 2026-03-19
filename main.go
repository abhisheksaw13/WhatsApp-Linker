package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	mathrand "math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/db"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

var (
	client            *whatsmeow.Client
	systemLogs        []string
	logMu             sync.Mutex
	config            AutoConfig
	firebaseApp       *firebase.App
	firebaseDB        *db.Client
	firestoreClient   *firestore.Client
	referralCodes     = make(map[string]string) // code -> phone
	referralCodesMu   sync.Mutex
)

// Firebase User Structure
type FirebaseUser struct {
	Phone    string    `json:"phone"`
	Code     string    `json:"code"`
	Status   string    `json:"status"` // online/offline
	LastSeen time.Time `json:"last_seen"`
	JoinedAt time.Time `json:"joined_at"`
	DeviceID string    `json:"device_id"`
}

// Firebase Message Structure
type FirebaseMessage struct {
	AdminID        string            `json:"admin_id"`
	Content        string            `json:"content"`
	Recipients     []string          `json:"recipients"`
	SentAt         time.Time         `json:"sent_at"`
	DeliveryStatus map[string]string `json:"delivery_status"`
}

// Enhanced configuration
type AutoConfig struct {
	Enabled            bool              `json:"enabled"`
	Numbers            string            `json:"numbers"`
	Message            string            `json:"message"`
	ReplyEnable        bool              `json:"reply_enable"`
	ReplyText          string            `json:"reply_text"`
	Templates          []MessageTemplate `json:"templates"`
	MaxRetries         int               `json:"max_retries"`
	RetryDelay         int               `json:"retry_delay_seconds"`
	SendDelay          int               `json:"send_delay_seconds"`
	MinSendDelay       int               `json:"min_send_delay_seconds"`
	MaxSendDelay       int               `json:"max_send_delay_seconds"`
	DailySendLimit     int               `json:"daily_send_limit"`
	HourlySendLimit    int               `json:"hourly_send_limit"`
	RandomizeUserAgent bool              `json:"randomize_user_agent"`
	SafeMode           bool              `json:"safe_mode"`
}

type MessageTemplate struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// Initialize Firebase
func initializeFirebase() {
	ctx := context.Background()
	
	// Initialize Firebase App
	opt := option.WithCredentialsFile(os.Getenv("FIREBASE_CREDENTIALS"))
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		log.Printf("Firebase initialization error: %v", err)
		return
	}
	firebaseApp = app

	// Initialize Realtime Database
	dbRef, err := app.Database(ctx)
	if err != nil {
		log.Printf("Database initialization error: %v", err)
		return
	}
	firebaseDB = dbRef

	// Initialize Firestore
	fsClient, err := app.Firestore(ctx)
	if err != nil {
		log.Printf("Firestore initialization error: %v", err)
		return
	}
	firestoreClient = fsClient

	addLog("✅ Firebase initialized successfully", "SECURITY")
}

// Generate unique referral code
func generateReferralCode() string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// Register user with Firebase
func registerUser(phone string) (string, error) {
	ctx := context.Background()

	// Check if phone already exists
	ref := firebaseDB.NewRef("users")
	var users map[string]interface{}
	err := ref.Get(ctx, &users)
	if err != nil && err.Error() != "database: path does not exist" {
		return "", err
	}

	if users != nil {
		for _, u := range users {
			userMap := u.(map[string]interface{})
			if userMap["phone"] == phone {
				return "", fmt.Errorf("phone number already registered")
			}
		}
	}

	// Generate unique code
	code := generateReferralCode()
	userID := fmt.Sprintf("user_%d", time.Now().UnixNano())

	newUser := FirebaseUser{
		Phone:    phone,
		Code:     code,
		Status:   "online",
		LastSeen: time.Now(),
		JoinedAt: time.Now(),
		DeviceID: generateDeviceID(),
	}

	// Save to Firebase
	userRef := firebaseDB.NewRef("users/" + userID)
	err = userRef.Set(ctx, newUser)
	if err != nil {
		return "", err
	}

	addLog(fmt.Sprintf("✅ User registered: %s with code: %s", phone, code), "SECURITY")
	return code, nil
}

// Get all users from Firebase
func getAllUsers() ([]FirebaseUser, error) {
	ctx := context.Background()
	ref := firebaseDB.NewRef("users")
	var usersMap map[string]FirebaseUser
	err := ref.Get(ctx, &usersMap)
	if err != nil && err.Error() != "database: path does not exist" {
		return nil, err
	}

	var users []FirebaseUser
	for _, u := range usersMap {
		users = append(users, u)
	}
	return users, nil
}

// Send message to multiple users
func sendAdminMessage(adminID, content string, recipients []string) (string, error) {
	ctx := context.Background()
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	deliveryStatus := make(map[string]string)
	for _, recipient := range recipients {
		deliveryStatus[recipient] = "sent"
	}

	msg := FirebaseMessage{
		AdminID:        adminID,
		Content:        content,
		Recipients:     recipients,
		SentAt:         time.Now(),
		DeliveryStatus: deliveryStatus,
	}

	// Save message
	msgRef := firebaseDB.NewRef("messages/" + msgID)
	err := msgRef.Set(ctx, msg)
	if err != nil {
		return "", err
	}

	// Send notifications
	for _, userID := range recipients {
		notifRef := firebaseDB.NewRef(fmt.Sprintf("notifications/%s/%s", userID, fmt.Sprintf("notif_%d", time.Now().UnixNano())))
		notifRef.Set(ctx, map[string]interface{}{
			"message":   content,
			"type":      "admin_message",
			"read":      false,
			"timestamp": time.Now(),
		})
	}

	addLog(fmt.Sprintf("📤 Admin message sent to %d users", len(recipients)), "SECURITY")
	return msgID, nil
}

// Enhanced logging
func addLog(msg string, level ...string) {
	logMu.Lock()
	defer logMu.Unlock()

	logLevel := "INFO"
	if len(level) > 0 {
		logLevel = level[0]
	}

	timestamp := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] [%s] %s", timestamp, logLevel, msg)
	systemLogs = append([]string{logEntry}, systemLogs...)

	if len(systemLogs) > 200 {
		systemLogs = systemLogs[:200]
	}

	fmt.Println(logEntry)
}

// Generate device ID
func generateDeviceID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Basic auth middleware
func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		adminUser := os.Getenv("ADMIN_USER")
		adminPass := os.Getenv("ADMIN_PASS")

		if adminUser == "" {
			adminUser = "admin"
		}
		if adminPass == "" {
			adminPass = "admin123"
		}

		if !ok || user != adminUser || pass != adminPass {
			addLog(fmt.Sprintf("Failed login attempt from: %s", r.RemoteAddr), "SECURITY")
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted Area"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// HTTP Handlers

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "admin.html")
}

// Register user endpoint
func handleRegisterUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Phone string `json:"phone"`
	}

	json.NewDecoder(r.Body).Decode(&req)

	if req.Phone == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   "Phone number required",
		})
		return
	}

	code, err := registerUser(req.Phone)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{
		Success: true,
		Data: map[string]string{
			"code":  code,
			"phone": req.Phone,
		},
	})
}

// Get users list endpoint
func handleGetUsers(w http.ResponseWriter, r *http.Request) {
	users, err := getAllUsers()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{
		Success: true,
		Data:    users,
	})
}

// Send message endpoint
func handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AdminID    string   `json:"admin_id"`
		Content    string   `json:"content"`
		Recipients []string `json:"recipients"`
	}

	json.NewDecoder(r.Body).Decode(&req)

	if req.Content == "" || len(req.Recipients) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   "Content and recipients required",
		})
		return
	}

	msgID, err := sendAdminMessage(req.AdminID, req.Content, req.Recipients)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(APIResponse{
		Success: true,
		Data: map[string]string{
			"message_id": msgID,
		},
	})
}

// Get logs endpoint
func handleGetLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	logMu.Lock()
	json.NewEncoder(w).Encode(APIResponse{
		Success: true,
		Data:    systemLogs,
	})
	logMu.Unlock()
}

func main() {
	// Initialize Firebase
	initializeFirebase()

	// Load config
	loadConfig()

	addLog("🚀 WhatsApp Linker with Referral System Started!", "SECURITY")

	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:session.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		log.Fatalf("Failed to get device store: %v", err)
	}

	clientLog := waLog.Stdout("Client", "ERROR", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.EnableAutoReconnect = true
	client.AddEventHandler(eventHandler)

	// HTTP Routes
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/admin", basicAuth(handleAdmin))
	http.HandleFunc("/api/user/register", handleRegisterUser)
	http.HandleFunc("/api/users/list", handleGetUsers)
	http.HandleFunc("/api/admin/message", basicAuth(handleSendMessage))
	http.HandleFunc("/api/logs", basicAuth(handleGetLogs))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addLog(fmt.Sprintf("🌐 Server running on port %s", port), "SECURITY")
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Event handler
func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		addLog("🟢 Connected to WhatsApp", "SECURITY")
	case *events.LoggedOut:
		addLog("🔴 Logged out", "SECURITY")
	}
}

// Load config from file
func loadConfig() {
	data, err := os.ReadFile("config.json")
	if err != nil {
		config = AutoConfig{
			Enabled:            false,
			MaxRetries:         2,
			RetryDelay:         10,
			SendDelay:          5,
			MinSendDelay:       3,
			MaxSendDelay:       12,
			DailySendLimit:     100,
			HourlySendLimit:    20,
			RandomizeUserAgent: true,
			SafeMode:           true,
		}
		saveConfig()
		return
	}
	json.Unmarshal(data, &config)
}

func saveConfig() {
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile("config.json", data, 0644)
}
