package main

import (
	"fmt"
	"net/http"
)

const mcpVaultRehydrateFeature = "mcp_vault_rehydrate"
const mcpVaultRehydrateName = "heurchain_vault_rehydrate"

var mcpVaultRehydrateTool = map[string]interface{}{
	"name":        mcpVaultRehydrateName,
	"description": "Restore one archived memory in your tenant to active retrieval using its preserved embedding. Use only when reactivation is intended.",
	"inputSchema": map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"doc_id": map[string]string{"type": "string", "description": "Archived document ID."}},
		"required":   []string{"doc_id"},
	},
}

func (s *Server) availableMCPTools() []map[string]interface{} {
	if !s.config.Features[mcpVaultRehydrateFeature] {
		return mcpToolList
	}
	// Never append into the shared registry's backing array.
	return append(mcpToolList[:len(mcpToolList):len(mcpToolList)], mcpVaultRehydrateTool)
}

// Only wired through the guarded MCP dispatcher; not a public HTTP route.
func (s *Server) handleMCPVaultRehydrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DocID string `json:"doc_id"`
	}
	tenant, key, ok := s.memoryRequest(w, r, &req)
	if !ok {
		return
	}
	if !validID.MatchString(req.DocID) {
		jsonError(w, "valid doc_id required", http.StatusBadRequest)
		return
	}
	s.finishVaultRehydrate(w, r, tenant, req.DocID, fmt.Sprintf("apikey:%d", key))
}
