package appgroup

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/helper/policyutil"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/logical"
)

const (
	selectorTypeApp        = "app"
	selectorTypeGroup      = "group"
	selectorTypeSuperGroup = "supergroup"
)

// userIDStorageEntry represents the information stored in storage when a UserID is created.
// The structure of the UserID storage entry is the same for all the types of UserIDs generated.
type userIDStorageEntry struct {
	// Number of times this UserID can be used to perform the login operation
	NumUses int `json:"num_uses" structs:"num_uses" mapstructure:"num_uses"`

	// Duration after which this UserID should expire. This is capped by the backend mount's
	// max TTL value.
	UserIDTTL time.Duration `json:"userid_ttl" structs:"userid_ttl" mapstructure:"userid_ttl"`

	// The time in UTC when the UserID was created
	CreationTime time.Time `json:"creation_time" structs:"creation_time" mapstructure:"creation_time"`

	// The time in UTC when the UserID becomes eligible for tidy operation.
	// Tidying is performed by the PeriodicFunc of the backend which is 1 minute apart.
	ExpirationTime time.Time `json:"expiration_time" structs:"expiration_time" mapstructure:"expiration_time"`

	// The time in UTC representing the last time this storage entry was modified
	LastUpdatedTime time.Time `json:"last_updated_time" structs:"last_updated_time" mapstructure:"last_updated_time"`
}

// validationResponse will be the result of credentials verification performed during login.
// This contains information that either needs to be returned to the client or information
// required to be stored as metadata in the response, and/or the information required to
// create the client token.
type validationResponse struct {
	SelectorType  string        `json:"selector_type" structs:"selector_type" mapstructure:"selector_type"`
	SelectorValue string        `json:"selector_value" structs:"selector_value" mapstructure:"selector_value"`
	TokenTTL      time.Duration `json:"token_ttl" structs:"token_ttl" mapstructure:"token_ttl"`
	TokenMaxTTL   time.Duration `json:"token_max_ttl" structs:"token_max_ttl" mapstructure:"token_max_ttl"`
	Policies      []string      `json:"policies" structs:"policies" mapstructure:"policies"`
}

// Identifies the supplied selector and validates it, checks if the supplied user ID
// has a corresponding entry in the backend and udpates the use count if needed.
func (b *backend) validateCredentials(s logical.Storage, selector, userID string) (*validationResponse, error) {
	if selector == "" {
		return nil, fmt.Errorf("missing selector")
	}
	if userID == "" {
		return nil, fmt.Errorf("missing userID")
	}

	// From the selector field supplied as the credential, detect the type of UserID
	// supplied. UserID will be verified based on the type.
	selectorType := ""
	selectorValue := ""
	switch {
	case selector == selectorTypeSuperGroup:
		selectorType = selectorTypeSuperGroup
		selectorValue = b.salt.SaltID(userID)
	case strings.HasPrefix(selector, "app/") || strings.HasPrefix(selector, "group/"):
		selectorFields := strings.SplitN(selector, "/", 2)
		if len(selectorFields) != 2 {
			return nil, fmt.Errorf("invalid selector; selector type and value could not be parsed")
		}
		selectorType = strings.TrimSpace(selectorFields[0])
		selectorValue = strings.TrimSpace(selectorFields[1])
		if selectorValue == "" {
			return nil, fmt.Errorf("missing selector value")
		}
	default:
		return nil, fmt.Errorf("unrecognized selector")
	}

	// Do the selector validation first. If this results in an error, the UserID
	// entry should not be modified. Return the validation response if the UserID
	// is found to be valid and if the UserID entry is updated properly.
	validationResp, err := b.validateSelector(s, selectorType, selectorValue)
	if err != nil {
		return nil, err
	}

	// Check if the user ID supplied is valid. If use limit was specified
	// on the user ID, it will be decremented in this call.
	valid, err := b.userIDEntryValid(s, selectorType, selectorValue, userID)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("user ID not found under the %s selector type", selectorType)
	}

	return validationResp, nil
}

// Check if there exists an entry in the name of selectorValue for the selectorType supplied.
func (b *backend) validateSelector(s logical.Storage, selectorType, selectorValue string) (*validationResponse, error) {
	resp := &validationResponse{
		SelectorType:  selectorType,
		SelectorValue: selectorValue,
	}
	switch selectorType {
	case selectorTypeApp:
		app, err := b.appEntry(s, selectorValue)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, fmt.Errorf("app referred by the user ID does not exist")
		}
		resp.Policies = app.Policies
		resp.TokenTTL = app.TokenTTL
		resp.TokenMaxTTL = app.TokenMaxTTL
	case selectorTypeGroup:
		group, err := b.groupEntry(s, selectorValue)
		if err != nil {
			return nil, err
		}
		if group == nil {
			return nil, fmt.Errorf("group referred by the user ID does not exist")
		}
		groupPolicies, err := b.fetchPolicies(s, group.Apps)
		if err != nil {
			return nil, err
		}
		// Append the union of all the policies from all the apps on the group
		resp.Policies = append(resp.Policies, groupPolicies...)

		// Append the additional policies set on the group
		resp.Policies = append(resp.Policies, group.AdditionalPolicies...)

		resp.TokenTTL = group.TokenTTL
		resp.TokenMaxTTL = group.TokenMaxTTL
	case selectorTypeSuperGroup:
		superGroup, err := b.superGroupEntry(s, selectorValue)
		if err != nil {
			return nil, err
		}
		if superGroup == nil {
			return nil, fmt.Errorf("supergroup credential referred by the user ID does not exist")
		}
		for _, groupName := range superGroup.Groups {
			group, err := b.groupEntry(s, groupName)
			if err != nil {
				return nil, err
			}
			groupPolicies, err := b.fetchPolicies(s, group.Apps)
			if err != nil {
				return nil, err
			}
			// Append the union of all the policies from all the apps on the group
			resp.Policies = append(resp.Policies, groupPolicies...)
			// Append the additional policies set on the group
			resp.Policies = append(resp.Policies, group.AdditionalPolicies...)
		}

		for _, appName := range superGroup.Apps {
			app, err := b.appEntry(s, appName)
			if err != nil {
				return nil, err
			}
			// Append the policies set on the app
			resp.Policies = append(resp.Policies, app.Policies...)
		}

		// Append the additonal policies set on the supergroup entry
		resp.Policies = append(resp.Policies, superGroup.AdditionalPolicies...)

		resp.TokenTTL = superGroup.TokenTTL
		resp.TokenMaxTTL = superGroup.TokenMaxTTL
	default:
		return nil, fmt.Errorf("unknown selector type")
	}

	// Cap the token_ttl and token_max_ttl values.
	var err error
	resp.TokenTTL, resp.TokenMaxTTL, err = b.SanitizeTTL(resp.TokenTTL, resp.TokenMaxTTL)
	if err != nil {
		return nil, err
	}

	resp.Policies = policyutil.SanitizePolicies(resp.Policies)

	return resp, nil
}

// userIDEntryValid is used to determine if the given user ID is a valid one.
// The UserID is looked to be present only under the sub-view of the selector.
// This ensures that the UserIDs that are reused between selector types, the
// correct one is referred to. If the UserIDs are always generated by the
// backend, then there will be no collision between the UserIDs from different
// types. But, if same specific UserIDs are assigned across different selector
// types, then it should be supported.
func (b *backend) userIDEntryValid(s logical.Storage, selectorType, selectorValue, userID string) (bool, error) {
	// Prepare the storage index for the userID
	entryIndex := fmt.Sprintf("userid/%s/%s/%s", selectorType, selectorValue, b.salt.SaltID(strings.ToLower(userID)))
	// Acquire a lock to read/write userID
	lock := b.getUserIDLock(userID)

	lock.RLock()

	result := userIDStorageEntry{}
	if entry, err := s.Get(entryIndex); err != nil {
		lock.RUnlock()
		return false, err
	} else if entry == nil {
		lock.RUnlock()
		return false, nil
	} else if err := entry.DecodeJSON(&result); err != nil {
		lock.RUnlock()
		return false, err
	}

	// NumUses will be zero only if the usage limit was not set at all,
	// in which case, the UserID will remain to be valid as long as it is not
	// expired.
	if result.NumUses == 0 {
		lock.RUnlock()
		return true, nil
	}

	// If the NumUses is non-zero, it means that its use-count should be updated
	// in the storage. Switch the lock from a `read` to a `write` and update
	// the storage entry.
	lock.RUnlock()

	lock.Lock()
	defer lock.Unlock()

	// Lock switching may change the data. Refresh the contents.
	result = userIDStorageEntry{}
	if entry, err := s.Get(entryIndex); err != nil {
		return false, err
	} else if entry == nil {
		return false, nil
	} else if err := entry.DecodeJSON(&result); err != nil {
		return false, err
	}

	// If there exists a single use left, delete the UserID entry from
	// the storage but do not fail the validation request. Delete the
	// UserIDs lock from the map of locks. Subsequest requests to use
	// the same UserID will fail.
	if result.NumUses == 1 {
		if err := s.Delete(entryIndex); err != nil {
			return false, err
		}
		// The storage entry for superGroup type is not created by any endpoints
		// and it is not cleaned up in any other way. When the UserID belonging
		// to the superGroup storage entry is getting invalidated, the entry should
		// be deleted as well.
		if selectorType == selectorTypeSuperGroup {
			if err := b.deleteSuperGroupEntry(s, selectorValue); err != nil {
				return false, err
			}
		}
	} else {
		// If the use count is greater than one, decrement it and update the last updated time.
		result.NumUses -= 1
		result.LastUpdatedTime = time.Now().UTC()
		if entry, err := logical.StorageEntryJSON(entryIndex, &result); err != nil {
			return false, fmt.Errorf("failed to decrement the num_uses for user ID:%s", userID)
		} else if err = s.Put(entry); err != nil {
			return false, fmt.Errorf("failed to decrement the num_uses for user ID:%s", userID)
		}
	}

	return true, nil
}

func (b *backend) getUserIDLock(userID string) *sync.RWMutex {
	// Find our multilevel lock, or fall back to global
	var lock *sync.RWMutex
	var ok bool
	if len(userID) >= 2 {
		lock, ok = b.userIDLocksMap[userID[0:2]]
	}
	if !ok || lock == nil {
		// Fall back for custom user IDs
		lock = b.userIDLocksMap["custom"]
	}

	return lock
}

// registerUserIDEntry creates a new storage entry for the given UserID.
// Successful creation of the storage entry results in the creation of a
// lock in the map of locks maintained at the backend. The index into the
// map is the UserID itself. During login, if the UserID supplied is not
// having a corresponding lock in the map, the login attempt fails.
func (b *backend) registerUserIDEntry(s logical.Storage, selectorType, selectorValue, userID string, userIDEntry *userIDStorageEntry) error {

	// Prepare the storage index for the userID
	entryIndex := fmt.Sprintf("userid/%s/%s/%s", selectorType, selectorValue, b.salt.SaltID(strings.ToLower(userID)))

	// Acquire a lock to read/write userID
	lock := b.getUserIDLock(userID)

	// See if there is already an entry for the given UserID
	lock.RLock()
	entry, err := s.Get(entryIndex)
	if err != nil {
		lock.RUnlock()
		return err
	}
	if entry != nil {
		lock.RUnlock()
		return fmt.Errorf("user ID is already registered")
	}

	// If there isn't an entry for the userID already, switch the read lock
	// with a write lock and create an entry. But before saving a new entry,
	// check if the userID entry was created during the lock switch.
	lock.RUnlock()
	lock.Lock()
	defer lock.Unlock()

	entry, err = s.Get(entryIndex)
	if err != nil {
		return err
	}
	if entry != nil {
		return fmt.Errorf("user ID is already registered")
	}

	// UserID was not created during the lock switch. Create a new entry.

	// Set the creation time for the UserID
	currentTime := time.Now().UTC()
	userIDEntry.CreationTime = currentTime
	userIDEntry.LastUpdatedTime = currentTime

	// If UserIDTTL is not specified or if it crosses the backend mount's limit,
	// cap the expiration to backend's max. Otherwise, use it to determine the
	// expiration time.
	if userIDEntry.UserIDTTL < time.Duration(0) || userIDEntry.UserIDTTL > b.System().MaxLeaseTTL() {
		userIDEntry.ExpirationTime = currentTime.Add(b.System().MaxLeaseTTL())
	} else if userIDEntry.UserIDTTL != time.Duration(0) {
		// Set the ExpirationTime only if UserIDTTL was set. UserIDs should not
		// expire by default.
		userIDEntry.ExpirationTime = currentTime.Add(userIDEntry.UserIDTTL)
	}

	if entry, err := logical.StorageEntryJSON(entryIndex, userIDEntry); err != nil {
		return err
	} else if err = s.Put(entry); err != nil {
		return err
	}

	return nil
}

// Iterates through all the Apps, fetches the polices from each App
// and returns a union of all the policies combined together.
// An error is thrown if any App in the list of Apps supplied
// is non-existent at the backend.
func (b *backend) fetchPolicies(s logical.Storage, apps []string) ([]string, error) {
	var policies []string
	for _, appName := range apps {
		app, err := b.appEntry(s, appName)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, fmt.Errorf("app %s does not exist", appName)
		}
		// Append the policies of each App into a collection
		policies = append(policies, app.Policies...)
	}
	return strutil.RemoveDuplicates(policies), nil
}