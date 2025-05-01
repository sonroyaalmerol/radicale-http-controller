package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type RadicaleConfig struct {
	Rights   []Right   `json:"rights"`
	Symlinks []Symlink `json:"symlinks"`
}

type ControllerConfig struct {
	ConfigURL                string
	PollInterval             time.Duration
	RightsFilePath           string
	RadicaleStoragePath      string
	HttpMethod               string
	AuthType                 string
	AuthUsername             string
	AuthPassword             string
	AuthToken                string
	ConfigPath               string
	RightsConfigMapName      string
	RightsConfigMapNamespace string
	RightsConfigMapKey       string
	IsInKubernetes           bool
	KubeClient               *kubernetes.Clientset
}

var (
	currentConfig *RadicaleConfig
	httpClient    = &http.Client{Timeout: 15 * time.Second}
)

const podNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	valueStr := getEnv(key, "")
	if valueStr == "" {
		return fallback
	}
	duration, err := time.ParseDuration(valueStr)
	if err != nil {
		log.Printf(
			"WARN: Invalid duration format for %s: '%s'. Using default: %v. Error: %v",
			key,
			valueStr,
			fallback,
			err,
		)
		return fallback
	}
	return duration
}

func detectKubernetes() (bool, *kubernetes.Clientset, string) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		log.Print("INFO: KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT not set. Assuming not in Kubernetes.")
		return false, nil, ""
	}

	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		log.Printf("INFO: Service account token '%s' not found. Assuming not in Kubernetes.", tokenPath)
		return false, nil, ""
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("ERROR: Failed to get in-cluster Kubernetes config: %v", err)
		return false, nil, ""
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("ERROR: Failed to create Kubernetes clientset: %v", err)
		return false, nil, ""
	}

	namespaceBytes, err := os.ReadFile(podNamespacePath)
	namespace := string(namespaceBytes)
	if err != nil {
		log.Printf("WARN: Could not read pod namespace file '%s', defaulting namespace resolution. Error: %v", podNamespacePath, err)
		namespace = "default" // Or leave empty and rely on env var
	}
	namespace = strings.TrimSpace(namespace)

	log.Printf("INFO: Detected Kubernetes environment. Default namespace: '%s'", namespace)
	return true, clientset, namespace
}

func loadConfiguration() *ControllerConfig {
	isInK8s, k8sClient, defaultNs := detectKubernetes()

	cfg := &ControllerConfig{
		ConfigURL:                getEnv("CONFIG_URL", ""),
		PollInterval:             getEnvDuration("POLL_INTERVAL", 5*time.Minute),
		RightsFilePath:           getEnv("RIGHTS_FILE_PATH", "/etc/radicale/rights"),
		RadicaleStoragePath:      getEnv("RADICALE_STORAGE_PATH", ""),
		HttpMethod:               strings.ToUpper(getEnv("HTTP_METHOD", "GET")),
		AuthType:                 strings.ToLower(getEnv("AUTH_TYPE", "none")),
		AuthUsername:             getEnv("AUTH_USERNAME", ""),
		AuthPassword:             getEnv("AUTH_PASSWORD", ""),
		AuthToken:                getEnv("AUTH_TOKEN", ""),
		ConfigPath:               getEnv("CONFIG_PATH", "radicale"),
		RightsConfigMapName:      getEnv("RIGHTS_CONFIGMAP_NAME", ""),
		RightsConfigMapNamespace: getEnv("RIGHTS_CONFIGMAP_NAMESPACE", defaultNs),
		RightsConfigMapKey:       getEnv("RIGHTS_CONFIGMAP_KEY", "rights"),
		IsInKubernetes:           isInK8s,
		KubeClient:               k8sClient,
	}

	if cfg.ConfigURL == "" {
		log.Fatal("FATAL: CONFIG_URL environment variable is not set.")
	}
	if cfg.IsInKubernetes && cfg.RightsConfigMapName == "" {
		log.Print("WARN: Running in Kubernetes but RIGHTS_CONFIGMAP_NAME is not set. ConfigMap writing disabled.")
		cfg.IsInKubernetes = false // Disable k8s features if CM name is missing
	}
	if cfg.IsInKubernetes && cfg.RightsConfigMapNamespace == "" {
		log.Print("WARN: Running in Kubernetes but could not determine namespace and RIGHTS_CONFIGMAP_NAMESPACE is not set. Using 'default'.")
		cfg.RightsConfigMapNamespace = "default"
	}

	log.Printf("--- Controller Configuration ---")
	log.Printf("Config URL: %s", cfg.ConfigURL)
	log.Printf("Poll Interval: %v", cfg.PollInterval)
	log.Printf("Rights File Path: %s", cfg.RightsFilePath)
	log.Printf("Radicale Storage Path: %s", cfg.RadicaleStoragePath)
	log.Printf("HTTP Method: %s", cfg.HttpMethod)
	log.Printf("Auth Type: %s", cfg.AuthType)
	log.Printf("Config Path in JSON: %s", cfg.ConfigPath)
	log.Printf("Running in Kubernetes: %t", cfg.IsInKubernetes)
	if cfg.IsInKubernetes {
		log.Printf("Rights ConfigMap Name: %s", cfg.RightsConfigMapName)
		log.Printf("Rights ConfigMap Namespace: %s", cfg.RightsConfigMapNamespace)
		log.Printf("Rights ConfigMap Key: %s", cfg.RightsConfigMapKey)
	}
	log.Printf("------------------------------")

	return cfg
}

func getConfigAtPath(data map[string]interface{}, path string) (interface{}, error) {
	parts := strings.Split(path, ".")
	var current interface{} = data

	for _, part := range parts {
		if part == "" {
			continue
		}
		currentMap, ok := current.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf(
				"path element '%s' is not a map/object in the JSON structure",
				part,
			)
		}
		value, exists := currentMap[part]
		if !exists {
			return nil, fmt.Errorf(
				"path element '%s' not found in the JSON structure",
				part,
			)
		}
		current = value
	}
	return current, nil
}

func mapToRadicaleConfig(data interface{}) (*RadicaleConfig, error) {
	configMap, ok := data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf(
			"the data at the specified config path is not a JSON object",
		)
	}

	var config RadicaleConfig

	if rightsData, ok := configMap["rights"]; ok {
		rightsSlice, ok := rightsData.([]interface{})
		if !ok {
			return nil, fmt.Errorf("field 'rights' is not an array")
		}
		for i, item := range rightsSlice {
			rightMap, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("item %d in 'rights' is not an object", i)
			}
			var right Right
			if v, ok := rightMap["section"].(string); ok {
				right.Section = v
			}
			if v, ok := rightMap["user"].(string); ok {
				right.User = v
			}
			if v, ok := rightMap["collection"].(string); ok {
				right.Collection = v
			}
			if v, ok := rightMap["permissions"].(string); ok {
				right.Permissions = v
			}
			config.Rights = append(config.Rights, right)
		}
	} else {
		log.Print("WARN: 'rights' field not found in the config data")
	}

	if symlinksData, ok := configMap["symlinks"]; ok {
		symlinksSlice, ok := symlinksData.([]interface{})
		if !ok {
			return nil, fmt.Errorf("field 'symlinks' is not an array")
		}
		for i, item := range symlinksSlice {
			symlinkMap, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("item %d in 'symlinks' is not an object", i)
			}
			var symlink Symlink
			if v, ok := symlinkMap["source"].(string); ok {
				symlink.Source = v
			}
			if v, ok := symlinkMap["user"].(string); ok {
				symlink.User = v
			}
			config.Symlinks = append(config.Symlinks, symlink)
		}
	} else {
		log.Print("WARN: 'symlinks' field not found in the config data")
	}

	return &config, nil
}

func fetchConfig(cfg *ControllerConfig) (*RadicaleConfig, error) {
	log.Printf("Fetching configuration from %s", cfg.ConfigURL)

	var reqBody io.Reader = nil

	req, err := http.NewRequest(cfg.HttpMethod, cfg.ConfigURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	switch cfg.AuthType {
	case "basic":
		if cfg.AuthUsername == "" {
			log.Print(
				"WARN: Auth type is 'basic' but AUTH_USERNAME is not set.",
			)
		}
		req.SetBasicAuth(cfg.AuthUsername, cfg.AuthPassword)
		log.Print("Using Basic Authentication")
	case "bearer":
		if cfg.AuthToken == "" {
			log.Print("WARN: Auth type is 'bearer' but AUTH_TOKEN is not set.")
		}
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
		log.Print("Using Bearer Token Authentication")
	case "none":
		log.Print("Using no authentication")
	default:
		log.Printf(
			"WARN: Unknown AUTH_TYPE '%s'. Proceeding without authentication.",
			cfg.AuthType,
		)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"failed to fetch config: received status code %d. Body: %s",
			resp.StatusCode,
			string(bodyBytes),
		)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var rawJsonData map[string]interface{}
	err = json.Unmarshal(bodyBytes, &rawJsonData)
	if err != nil {
		log.Printf("DEBUG: Failed to unmarshal body: %s", string(bodyBytes))
		return nil, fmt.Errorf("failed to unmarshal base JSON: %w", err)
	}

	radicaleData, err := getConfigAtPath(rawJsonData, cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to extract config using path '%s': %w",
			cfg.ConfigPath,
			err,
		)
	}

	config, err := mapToRadicaleConfig(radicaleData)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to map extracted data to RadicaleConfig: %w",
			err,
		)
	}

	log.Printf(
		"Successfully fetched and parsed configuration (%d rights, %d symlinks)",
		len(config.Rights),
		len(config.Symlinks),
	)
	return config, nil
}
