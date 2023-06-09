package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// KeyValue represents a key-value pair in the datastore.
// It stores the value and an optional expiry time for the key.
type KeyValue struct {
	Value      []string   // The value associated with the key
	ExpiryTime *time.Time // The expiry time for the key (optional)
}

// KeyValueStore represents an in-memory key-value data store.
// It stores the data and provides thread-safe access using a mutex.
type KeyValueStore struct {
	Data  map[string]*KeyValue // The underlying data store
	mutex sync.RWMutex         // Mutex for thread-safe access to the data store
}

// Mutex : Primitive used in concurrent programming to protect shared resources
// from being accessed simultaneously by multiple threads or goroutines

type Command struct {
	Command string `json:"command"` // Represents a JSON command received via the REST API.
}

type ErrorResponse struct {
	Error string `json:"error"` // Represents a JSON response containing an error message.
}

type ValueResponse struct {
	Value string `json:"value"` // Represents a JSON response containing a value.
}

type QueueOperation struct {
	Operation      string
	Key            string
	Values         []string
	Response       chan string
	ResponseWriter http.ResponseWriter
}

var store = &KeyValueStore{
	Data: make(map[string]*KeyValue), // Initializes the key-value data store.
}

var queueChannel = make(chan QueueOperation)
var queueListeners sync.WaitGroup

var ctx = context.Background()
var RedisClient *redis.Client

func init() {
	RedisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379", // Update with your Redis server address and port
		Password: "",               // Set if authentication is required
		DB:       0,                // Select the appropriate database index
	})
}

func main() {
	go handleQueueOperations()

	http.HandleFunc("/", handleRequest) // Sets up the request handler
	http.ListenAndServe(":8080", nil)   // Starts the HTTP server and listens on port 8080.
}

// Sends error response to the client.
func sendErrorResponse(w http.ResponseWriter, errorMessage string) {
	// Create ErrorResponse object as JSON with the specified error message.
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(ErrorResponse{Error: errorMessage})
}

// Sends a value response.
func sendValueResponse(w http.ResponseWriter, value string) {
	// CreateValueResponse object as JSON with the specified value.
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(ValueResponse{Value: value})
}

// Sends a simple OK response to the client.
func sendOKResponse(w http.ResponseWriter) {
	// Send an empty response as JSON to indicate a successful response.
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct{}{})
}

// ResponseWrites helps to onstruct and send response back to client
// Request represents incoming HTTP requests recieved from client

func handleRequest(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body) //Decoder to decode request body into "Command" struct
	defer r.Body.Close()               //Request body is closed after request is processed

	var cmd Command
	err := decoder.Decode(&cmd)
	if err != nil {
		sendErrorResponse(w, "invalid request")
		return
	}

	parts := strings.Split(cmd.Command, " ") //Splits the command string into parts
	if len(parts) == 0 {
		sendErrorResponse(w, "invalid command")
		return
	}
	//First index is converted to uppercase and performed a switch statement to trigger appropriate function.
	switch strings.ToUpper(parts[0]) {
	case "SET":
		handleSET(w, parts)
	case "GET":
		handleGET(w, parts)
	case "QPUSH":
		handleQPUSH(w, parts)
	case "QPOP":
		handleQPOP(w, parts)
	case "BQPOP":
		handleBQPOP(w, parts) //Optional
	default:
		sendErrorResponse(w, "invalid command")
	}
}

func handleSET(w http.ResponseWriter, parts []string) {
	if len(parts) < 3 {
		sendErrorResponse(w, "invalid command format")
		return
	}

	key := parts[1]   //sets key
	value := parts[2] // sets value

	// Currently - empty initialization
	var expiryTime time.Time
	var condition string

	if len(parts) >= 4 && strings.HasPrefix(parts[3], "EX") {
		// extracts the number of seconds for the expiry time, converts it to an integer
		// sets the expiryTime variable to the current time plus the specified duration.
		seconds, err := strconv.Atoi(parts[3][2:])
		if err != nil {
			sendErrorResponse(w, "invalid expiry time")
			return
		}
		expiryTime = time.Now().Add(time.Duration(seconds) * time.Second)
	}

	if len(parts) == 5 {
		condition = strings.ToUpper(parts[4])
		if condition != "NX" && condition != "XX" {
			sendErrorResponse(w, "invalid condition")
			return
		}
	}
	// Makes sure only one process can use the store at one time
	// To Support Concurrent Operations
	store.mutex.Lock() //write lock

	defer store.mutex.Unlock()

	if condition == "NX" {
		if _, ok := store.Data[key]; ok {
			sendErrorResponse(w, "key already exists")
			return
		}
	} else if condition == "XX" {
		if _, ok := store.Data[key]; !ok {
			sendErrorResponse(w, "key does not exist")
			return
		}
	}

	store.Data[key] = &KeyValue{
		Value:      []string{value},
		ExpiryTime: &expiryTime,
	}

	sendOKResponse(w)
}

// retrieves the value associated with a given key from the data store, ensuring concurrent access using a mutex lock.
func handleGET(w http.ResponseWriter, parts []string) {
	if len(parts) != 2 {
		sendErrorResponse(w, "invalid command format")
		return
	}

	key := parts[1]

	// Makes sure only one process can use the store at one time
	// To Support Concurrent Operations
	store.mutex.RLock()
	defer store.mutex.RUnlock()

	if kv, ok := store.Data[key]; ok {
		value := strings.Join(kv.Value, " ") // Convert the []string to a string
		sendValueResponse(w, value)
		return
	}

	sendErrorResponse(w, "key not found")
}

func handleQPUSH(w http.ResponseWriter, parts []string) {
	if len(parts) < 3 {
		sendErrorResponse(w, "invalid command format")
		return
	}

	key := parts[1]
	values := parts[2:]

	queueChannel <- QueueOperation{
		Operation: "QPUSH",
		Key:       key,
		Values:    values,
		Response:  make(chan string),
	}

	sendOKResponse(w)
}

func handleQPOP(w http.ResponseWriter, parts []string) {
	if len(parts) != 2 {
		sendErrorResponse(w, "invalid command format")
		return
	}

	key := parts[1]

	responseChan := make(chan string)

	queueChannel <- QueueOperation{
		Operation:      "QPOP",
		Key:            key,
		Response:       responseChan,
		ResponseWriter: w,
	}

	response := <-responseChan
	if response != "" {
		sendValueResponse(w, response)
	} else {
		sendErrorResponse(w, "queue is empty")
	}
}

func handleBQPOP(w http.ResponseWriter, parts []string) {
	if len(parts) != 2 {
		sendErrorResponse(w, "invalid command format")
		return
	}

	key := parts[1]

	responseChan := make(chan string)

	queueChannel <- QueueOperation{
		Operation:      "BQPOP",
		Key:            key,
		Response:       responseChan,
		ResponseWriter: w,
	}

	response := <-responseChan
	if response != "" {
		sendValueResponse(w, response)
	} else {
		sendErrorResponse(w, "queue is empty")
	}
}

// handleQueueOperations listens to the queueChannel and performs queue operations accordingly.
func handleQueueOperations() {
	for operation := range queueChannel {
		switch operation.Operation {
		case "QPUSH":
			handleQueuePush(operation)
		case "QPOP":
			handleQueuePop(operation)
		case "BQPOP":
			handleBlockingQueuePop(operation)
		}
	}
}

// handleQueuePush adds values to a queue associated with a given key.
func handleQueuePush(operation QueueOperation) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	key := operation.Key
	values := operation.Values

	if kv, ok := store.Data[key]; ok {
		kv.Value = append(kv.Value, values...)
	} else {
		store.Data[key] = &KeyValue{
			Value: values,
		}
	}

	operation.Response <- "" // Empty response to indicate success
}

// handleQueuePop removes and returns the first value from a queue associated with a given key.
func handleQueuePop(operation QueueOperation) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	key := operation.Key

	if kv, ok := store.Data[key]; ok {
		if len(kv.Value) > 0 {
			value := kv.Value[0]
			kv.Value = kv.Value[1:]

			operation.Response <- value
			return
		}
	}

	operation.Response <- "" // Empty response to indicate failure
}

// handleBlockingQueuePop removes and returns the first value from a queue associated with a given key.
// If the queue is empty, it blocks until a value is added or the timeout is reached.
func handleBlockingQueuePop(operation QueueOperation) {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	key := operation.Key

	if kv, ok := store.Data[key]; ok {
		if len(kv.Value) > 0 {
			value := kv.Value[0]
			kv.Value = kv.Value[1:]

			operation.Response <- value
			return
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sub := RedisClient.Subscribe(timeoutCtx, key)

	defer sub.Close()

	for {
		msg, err := sub.ReceiveMessage(timeoutCtx)
		if err != nil {
			operation.Response <- "" // Empty response to indicate failure
			return
		}

		if msg.Channel == key {
			operation.Response <- msg.Payload
			return
		}
	}
}
