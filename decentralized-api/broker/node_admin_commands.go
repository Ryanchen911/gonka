package broker

import (
	"decentralized-api/apiconfig"
	"decentralized-api/logging"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/productscience/inference/x/inference/types"
)

type RegisterNode struct {
	Node     apiconfig.InferenceNodeConfig
	Response chan *apiconfig.InferenceNodeConfig
}

func (r RegisterNode) GetResponseChannelCapacity() int {
	return cap(r.Response)
}

// validateInferenceNodeConfig validates node configuration:
// - Requires either (Host+Ports) OR baseURL, not both
// - baseURL must be valid HTTP(S) URL
// - AuthToken is always optional (no validation needed)
func validateInferenceNodeConfig(node apiconfig.InferenceNodeConfig) error {
	hasHostPorts := strings.TrimSpace(node.Host) != "" && node.InferencePort > 0 && node.PoCPort > 0
	hasBaseURL := strings.TrimSpace(node.BaseURL) != ""

	if hasHostPorts && hasBaseURL {
		return fmt.Errorf("node configuration error: cannot specify both (Host+Ports) and baseURL. Use either Host+InferencePort+PoCPort OR baseURL")
	}

	if !hasHostPorts && !hasBaseURL {
		return fmt.Errorf("node configuration error: must specify either (Host+InferencePort+PoCPort) OR baseURL")
	}

	if hasBaseURL {
		// Validate baseURL is a valid HTTP(S) URL
		parsedURL, err := url.Parse(node.BaseURL)
		if err != nil {
			return fmt.Errorf("node configuration error: baseURL is not a valid URL: %w", err)
		}

		if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
			return fmt.Errorf("node configuration error: baseURL must use http:// or https:// scheme, got: %s", parsedURL.Scheme)
		}

		if parsedURL.Host == "" {
			return fmt.Errorf("node configuration error: baseURL must include a valid host")
		}

		// Validate host is either a valid IP address or a valid domain name
		hostname := parsedURL.Hostname()
		if hostname == "" {
			return fmt.Errorf("node configuration error: baseURL must include a valid hostname")
		}

		// Check if it's a valid IP address
		if ip := net.ParseIP(hostname); ip != nil {
			// Valid IP address, allow it
		} else {
			// Not an IP, check if it's a valid domain name format
			// Basic validation: domain should contain at least one dot or be localhost
			if hostname != "localhost" && !strings.Contains(hostname, ".") {
				return fmt.Errorf("node configuration error: baseURL hostname '%s' is not a valid IP address or domain name", hostname)
			}
			// Additional check: domain should not start or end with dot or hyphen
			if strings.HasPrefix(hostname, ".") || strings.HasSuffix(hostname, ".") ||
				strings.HasPrefix(hostname, "-") || strings.HasSuffix(hostname, "-") {
				return fmt.Errorf("node configuration error: baseURL hostname '%s' has invalid format", hostname)
			}
		}
	}

	return nil
}

func (c RegisterNode) Execute(b *Broker) {
	// Validate node configuration
	if err := validateInferenceNodeConfig(c.Node); err != nil {
		logging.Error("RegisterNode. Invalid node configuration", types.Nodes, "error", err, "node_id", c.Node.Id)
		c.Response <- nil
		return
	}
	govModels, err := b.chainBridge.GetGovernanceModels()
	if err != nil {
		logging.Error("RegisterNode. Failed to get governance models", types.Nodes, "error", err)
		c.Response <- nil
		return
	}

	modelMap := make(map[string]struct{})
	for _, model := range govModels.Model {
		logging.Info("RegisterNode. Governance model", types.Nodes, "model_id", model.Id)
		modelMap[model.Id] = struct{}{}
	}

	for modelId := range c.Node.Models {
		if _, ok := modelMap[modelId]; !ok {
			logging.Error("RegisterNode. Model is not a valid governance model", types.Nodes, "model_id", modelId)
			c.Response <- nil
			return
		}
	}

	b.curMaxNodesNum.Add(1)
	curNum := b.curMaxNodesNum.Load()

	models := make(map[string]ModelArgs)
	for model, config := range c.Node.Models {
		models[model] = ModelArgs{Args: config.Args}
	}

	node := Node{
		Host:             c.Node.Host,
		InferenceSegment: c.Node.InferenceSegment,
		InferencePort:    c.Node.InferencePort,
		PoCSegment:       c.Node.PoCSegment,
		PoCPort:          c.Node.PoCPort,
		BaseURL:          c.Node.BaseURL,
		AuthToken:        c.Node.AuthToken,
		Models:           models,
		Id:               c.Node.Id,
		MaxConcurrent:    c.Node.MaxConcurrent,
		NodeNum:          curNum,
		Hardware:         c.Node.Hardware,
	}

	var currentEpoch uint64
	if b.phaseTracker != nil {
		epochState := b.phaseTracker.GetCurrentEpochState()
		if epochState == nil {
			currentEpoch = 0
		} else {
			currentEpoch = epochState.LatestEpoch.EpochIndex
		}
	}

	nodeWithState := &NodeWithState{
		Node: node,
		State: NodeState{
			IntendedStatus:    types.HardwareNodeStatus_UNKNOWN,
			CurrentStatus:     types.HardwareNodeStatus_UNKNOWN,
			ReconcileInfo:     nil,
			PocIntendedStatus: PocStatusIdle,
			PocCurrentStatus:  PocStatusIdle,
			LockCount:         0,
			FailureReason:     "",
			StatusTimestamp:   time.Now(),
			AdminState: AdminState{
				Enabled: true,
				Epoch:   currentEpoch,
			},
			EpochModels:  make(map[string]types.Model),
			EpochMLNodes: make(map[string]types.MLNodeInfo),
		},
	}

	func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.nodes[c.Node.Id] = nodeWithState

		// Create and register a worker for this node
		client := b.NewNodeClient(&node)
		worker := NewNodeWorkerWithClient(c.Node.Id, nodeWithState, client, b)
		b.nodeWorkGroup.AddWorker(c.Node.Id, worker)
	}()

	// Trigger a status check for the newly added node.
	b.TriggerStatusQuery()

	logging.Info("RegisterNode. Registered node", types.Nodes, "node", c.Node)
	c.Response <- &c.Node
}

// UpdateNode updates an existing node's configuration while preserving runtime state
type UpdateNode struct {
	Node     apiconfig.InferenceNodeConfig
	Response chan *apiconfig.InferenceNodeConfig
}

func NewUpdateNodeCommand(node apiconfig.InferenceNodeConfig) UpdateNode {
	return UpdateNode{
		Node:     node,
		Response: make(chan *apiconfig.InferenceNodeConfig, 2),
	}
}

func (u UpdateNode) GetResponseChannelCapacity() int {
	return cap(u.Response)
}

func (c UpdateNode) Execute(b *Broker) {
	// Validate node configuration
	if err := validateInferenceNodeConfig(c.Node); err != nil {
		logging.Error("UpdateNode. Invalid node configuration", types.Nodes, "error", err, "node_id", c.Node.Id)
		c.Response <- nil
		return
	}

	// Validate models exist in governance
	govModels, err := b.chainBridge.GetGovernanceModels()
	if err != nil {
		logging.Error("UpdateNode. Failed to get governance models", types.Nodes, "error", err)
		c.Response <- nil
		return
	}

	modelMap := make(map[string]struct{})
	for _, model := range govModels.Model {
		modelMap[model.Id] = struct{}{}
	}

	for modelId := range c.Node.Models {
		if _, ok := modelMap[modelId]; !ok {
			logging.Error("UpdateNode. Model is not a valid governance model", types.Nodes, "model_id", modelId)
			c.Response <- nil
			return
		}
	}

	// Fetch existing node
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, exists := b.nodes[c.Node.Id]
	if !exists {
		logging.Error("UpdateNode. Node not found", types.Nodes, "node_id", c.Node.Id)
		c.Response <- nil
		return
	}

	// Build updated Node struct, preserving node number
	models := make(map[string]ModelArgs)
	for model, config := range c.Node.Models {
		models[model] = ModelArgs{Args: config.Args}
	}

	updated := Node{
		Host:             c.Node.Host,
		InferenceSegment: c.Node.InferenceSegment,
		InferencePort:    c.Node.InferencePort,
		PoCSegment:       c.Node.PoCSegment,
		PoCPort:          c.Node.PoCPort,
		BaseURL:          c.Node.BaseURL,
		AuthToken:        c.Node.AuthToken,
		Models:           models,
		Id:               c.Node.Id,
		MaxConcurrent:    c.Node.MaxConcurrent,
		NodeNum:          existing.Node.NodeNum,
		Hardware:         c.Node.Hardware,
	}

	// Apply update
	existing.Node = updated

	// Optionally trigger a status re-check
	b.TriggerStatusQuery()

	logging.Info("UpdateNode. Updated node configuration", types.Nodes, "node_id", c.Node.Id)
	c.Response <- &c.Node
}

type RemoveNode struct {
	NodeId   string
	Response chan bool
}

func (r RemoveNode) GetResponseChannelCapacity() int {
	return cap(r.Response)
}

func (command RemoveNode) Execute(b *Broker) {
	// Remove the worker first (it will wait for pending jobs)
	b.nodeWorkGroup.RemoveWorker(command.NodeId)

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.nodes[command.NodeId]; !ok {
		command.Response <- false
		return
	}
	delete(b.nodes, command.NodeId)
	logging.Debug("Removed node", types.Nodes, "node_id", command.NodeId)
	command.Response <- true
}

// SetNodeAdminStateCommand enables or disables a node administratively
type SetNodeAdminStateCommand struct {
	NodeId   string
	Enabled  bool
	Response chan error
}

func (c SetNodeAdminStateCommand) GetResponseChannelCapacity() int {
	return cap(c.Response)
}

func (c SetNodeAdminStateCommand) Execute(b *Broker) {
	// Get current epoch
	var currentEpoch uint64
	if b.phaseTracker != nil {
		epochState := b.phaseTracker.GetCurrentEpochState()
		if epochState == nil {
			currentEpoch = 0
		} else {
			currentEpoch = epochState.LatestEpoch.EpochIndex
		}
	}

	b.mu.Lock()
	node, exists := b.nodes[c.NodeId]
	if !exists {
		c.Response <- fmt.Errorf("node not found: %s", c.NodeId)
		return
	}

	// Update admin state
	node.State.AdminState.Enabled = c.Enabled
	node.State.AdminState.Epoch = currentEpoch
	b.mu.Unlock()

	logging.Info("Updated node admin state", types.Nodes,
		"node_id", c.NodeId,
		"enabled", c.Enabled,
		"epoch", currentEpoch)

	c.Response <- nil
}

// UpdateNodeHardwareCommand updates the Hardware field for a specific node
type UpdateNodeHardwareCommand struct {
	NodeId   string
	Hardware []apiconfig.Hardware
	Response chan error
}

func (c UpdateNodeHardwareCommand) GetResponseChannelCapacity() int {
	return cap(c.Response)
}

func (c UpdateNodeHardwareCommand) Execute(b *Broker) {
	b.mu.Lock()
	defer b.mu.Unlock()

	node, exists := b.nodes[c.NodeId]
	if !exists {
		c.Response <- fmt.Errorf("node not found: %s", c.NodeId)
		return
	}

	node.Node.Hardware = c.Hardware
	logging.Info("Updated node hardware", types.Nodes, "node_id", c.NodeId, "hardware_count", len(c.Hardware))
	c.Response <- nil
}
