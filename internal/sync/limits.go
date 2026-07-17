package sync

const (
	MaxBlobBytes          int64 = 64 << 20
	MaxChangesRequestBody int64 = 2 << 20
	MaxChangeBatch              = 500
	MaxChangeDataBytes          = 256 << 10
	MaxChangesPageSize          = 1000
	MaxBlobsPageSize            = 1000
)

// Limits describes the fixed resource limits enforced by the sync API.
type Limits struct {
	MaxBlobBytes          int64 `json:"maxBlobBytes"`
	MaxChangesRequestBody int64 `json:"maxChangesRequestBytes"`
	MaxChangeBatch        int   `json:"maxChangesPerRequest"`
	MaxChangeDataBytes    int   `json:"maxChangeDataBytes"`
	MaxChangesPageSize    int   `json:"maxChangesPageSize"`
	MaxBlobsPageSize      int   `json:"maxBlobsPageSize"`
}

var syncLimits = Limits{
	MaxBlobBytes:          MaxBlobBytes,
	MaxChangesRequestBody: MaxChangesRequestBody,
	MaxChangeBatch:        MaxChangeBatch,
	MaxChangeDataBytes:    MaxChangeDataBytes,
	MaxChangesPageSize:    MaxChangesPageSize,
	MaxBlobsPageSize:      MaxBlobsPageSize,
}
