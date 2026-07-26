package relay

import (
	"errors"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

var ErrModelNotFound = errors.New("relay model not found")

type Candidate struct {
	Model      database.RelayModel
	Binding    database.RelayModelBinding
	Provider   database.RelayProvider
	Credential database.RelayProviderCredential
	Config     ProviderConfig
}

type cooldownState struct {
	until    time.Time
	failures int
}

type Router struct {
	db  *gorm.DB
	now func() time.Time
	rng *rand.Rand

	mu               sync.Mutex
	modelCredentials map[string]cooldownState
	credentials      map[int64]cooldownState
	bindings         map[int64]cooldownState
	cursors          map[string]int
}

func newRouter(db *gorm.DB) *Router {
	return newRouterWithSource(db, rand.NewSource(time.Now().UnixNano()))
}

func newRouterWithSource(db *gorm.DB, source rand.Source) *Router {
	return &Router{
		db: db, now: time.Now, rng: rand.New(source),
		modelCredentials: make(map[string]cooldownState), credentials: make(map[int64]cooldownState),
		bindings: make(map[int64]cooldownState), cursors: make(map[string]int),
	}
}

func (r *Router) bindingOrder(bindings []database.RelayModelBinding) []database.RelayModelBinding {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.weightedBindings(bindings)
}

func (r *Router) Candidates(modelID string) (database.RelayModel, []Candidate, error) {
	modelID = strings.TrimSpace(modelID)
	var model database.RelayModel
	if modelID == "" {
		return model, nil, ErrModelNotFound
	}
	if err := r.db.Where("model_id = ? AND enabled = ?", modelID, true).First(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model, nil, ErrModelNotFound
		}
		return model, nil, err
	}

	var bindings []database.RelayModelBinding
	if err := r.db.Where("relay_model_id = ? AND enabled = ?", model.ID, true).Order("id ASC").Find(&bindings).Error; err != nil {
		return model, nil, err
	}
	providerIDs := make([]int64, 0, len(bindings))
	for _, binding := range bindings {
		providerIDs = append(providerIDs, binding.ProviderID)
	}
	var providers []database.RelayProvider
	if len(providerIDs) > 0 {
		if err := r.db.Where("id IN ? AND enabled = ?", providerIDs, true).Find(&providers).Error; err != nil {
			return model, nil, err
		}
	}
	providersByID := make(map[int64]database.RelayProvider, len(providers))
	for _, provider := range providers {
		providersByID[provider.ID] = provider
	}
	var credentials []database.RelayProviderCredential
	if len(providerIDs) > 0 {
		if err := r.db.Where("provider_id IN ? AND enabled = ?", providerIDs, true).
			Order("priority DESC, id ASC").Find(&credentials).Error; err != nil {
			return model, nil, err
		}
	}
	credentialsByProvider := make(map[int64][]database.RelayProviderCredential)
	for _, credential := range credentials {
		credentialsByProvider[credential.ProviderID] = append(credentialsByProvider[credential.ProviderID], credential)
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	available := bindings[:0]
	for _, binding := range bindings {
		if !cooling(r.bindings[binding.ID], now) {
			available = append(available, binding)
		}
	}
	bindings = r.weightedBindings(available)

	candidates := make([]Candidate, 0)
	seenProviders := make(map[int64]struct{})
	for _, binding := range bindings {
		if _, seen := seenProviders[binding.ProviderID]; seen {
			continue
		}
		seenProviders[binding.ProviderID] = struct{}{}
		provider, exists := providersByID[binding.ProviderID]
		if !exists {
			continue
		}
		if !SupportsCategory(provider.APIFormat, model.Category) {
			continue
		}
		byPriority := make(map[int][]database.RelayProviderCredential)
		priorities := make([]int, 0)
		for _, credential := range credentialsByProvider[provider.ID] {
			key := modelCredentialKey(model.ID, credential.ID)
			if cooling(r.credentials[credential.ID], now) || cooling(r.modelCredentials[key], now) {
				continue
			}
			if _, exists := byPriority[credential.Priority]; !exists {
				priorities = append(priorities, credential.Priority)
			}
			byPriority[credential.Priority] = append(byPriority[credential.Priority], credential)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
		for _, priority := range priorities {
			group := byPriority[priority]
			cursorKey := strconv.FormatInt(provider.ID, 10) + ":" + strconv.Itoa(priority)
			start := r.cursors[cursorKey] % len(group)
			r.cursors[cursorKey] = (start + 1) % len(group)
			for i := range group {
				credential := group[(start+i)%len(group)]
				candidates = append(candidates, Candidate{
					Model: model, Binding: binding, Provider: provider, Credential: credential,
					Config: MergeProviderConfig(DecodeProviderConfig(provider.Config), DecodeProviderConfig(credential.Config)),
				})
			}
		}
	}
	if len(candidates) == 0 {
		return model, nil, nil
	}
	return model, candidates, nil
}

func (r *Router) weightedBindings(bindings []database.RelayModelBinding) []database.RelayModelBinding {
	remaining := append([]database.RelayModelBinding(nil), bindings...)
	ordered := make([]database.RelayModelBinding, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0
		for _, binding := range remaining {
			total += binding.Weight
		}
		pick := r.rng.Intn(total)
		index := 0
		for i, binding := range remaining {
			if pick < binding.Weight {
				index = i
				break
			}
			pick -= binding.Weight
		}
		ordered = append(ordered, remaining[index])
		remaining = append(remaining[:index], remaining[index+1:]...)
	}
	return ordered
}

func (r *Router) Success(candidate Candidate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.modelCredentials, modelCredentialKey(candidate.Model.ID, candidate.Credential.ID))
	delete(r.credentials, candidate.Credential.ID)
	delete(r.bindings, candidate.Binding.ID)
}

func (r *Router) Cooldown(candidate Candidate, status int, retryAfter string, network bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		r.bump(r.credentials, candidate.Credential.ID, now, retryAfter, 30*time.Second, 5*time.Minute)
		return
	}
	if status == http.StatusNotFound {
		r.bump(r.bindings, candidate.Binding.ID, now, retryAfter, 30*time.Second, 5*time.Minute)
		return
	}
	if network || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500 {
		base, capDuration := 30*time.Second, 5*time.Minute
		if status == http.StatusTooManyRequests {
			base, capDuration = time.Minute, 30*time.Minute
		}
		r.bumpModel(r.modelCredentials, modelCredentialKey(candidate.Model.ID, candidate.Credential.ID), now, retryAfter, base, capDuration)
	}
}

func (r *Router) ReleaseCredential(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.credentials, id)
	for key := range r.modelCredentials {
		if strings.HasSuffix(key, ":"+strconv.FormatInt(id, 10)) {
			delete(r.modelCredentials, key)
		}
	}
}

func (r *Router) bump(states map[int64]cooldownState, key int64, now time.Time, retryAfter string, base, capDuration time.Duration) {
	state := states[key]
	state.failures++
	state.until = now.Add(cooldownDuration(retryAfter, now, state.failures, base, capDuration))
	states[key] = state
}

func (r *Router) bumpModel(states map[string]cooldownState, key string, now time.Time, retryAfter string, base, capDuration time.Duration) {
	state := states[key]
	state.failures++
	state.until = now.Add(cooldownDuration(retryAfter, now, state.failures, base, capDuration))
	states[key] = state
}

func cooling(state cooldownState, now time.Time) bool { return state.until.After(now) }

func cooldownDuration(value string, now time.Time, failures int, base, capDuration time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(capDuration/time.Second) {
			return capDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		duration := at.Sub(now)
		if duration > capDuration {
			return capDuration
		}
		return duration
	}
	duration := base
	for i := 1; i < failures && duration < capDuration; i++ {
		duration *= 2
	}
	if duration > capDuration {
		return capDuration
	}
	return duration
}

func modelCredentialKey(modelID, credentialID int64) string {
	return strconv.FormatInt(modelID, 10) + ":" + strconv.FormatInt(credentialID, 10)
}
