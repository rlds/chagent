package channels

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"log"

	"github.com/rlds/chagent/pkg/config"
)

func toChannelHashes(cfg *config.Config) map[string]string {
	result := make(map[string]string)
	ch := cfg.Channels
	marshal, err := json.Marshal(ch)
	if err != nil {
		log.Printf("[manager_channel] failed to marshal channels config: %v", err)
		return result
	}
	var channelConfig map[string]map[string]any
	if err := json.Unmarshal(marshal, &channelConfig); err != nil {
		log.Printf("[manager_channel] failed to unmarshal channels config: %v", err)
		return result
	}

	for key, value := range channelConfig {
		if enabled, ok := value["enabled"].(bool); !ok || !enabled {
			continue
		}
		hiddenValues(key, value, ch.Get(key))
		valueBytes, err := json.Marshal(value)
		if err != nil {
			log.Printf("[manager_channel] failed to marshal channel %s config: %v", key, err)
			continue
		}
		hash := md5.Sum(valueBytes)
		result[key] = hex.EncodeToString(hash[:])
	}

	return result
}

func hiddenValues(key string, value map[string]any, ch *config.Channel) {
	v, err := ch.GetDecoded()
	if err != nil {
		return
	}
	switch key {
	case "pico":
		if settings, ok := v.(*config.PicoSettings); ok {
			value["token"] = settings.Token.String()
		}
	case "pico_client":
		if settings, ok := v.(*config.PicoClientSettings); ok {
			value["token"] = settings.Token.String()
		}
	}
}

func compareChannels(old, news map[string]string) (added, removed []string) {
	for key, newHash := range news {
		if oldHash, ok := old[key]; ok {
			if newHash != oldHash {
				removed = append(removed, key)
				added = append(added, key)
			}
		} else {
			added = append(added, key)
		}
	}
	for key := range old {
		if _, ok := news[key]; !ok {
			removed = append(removed, key)
		}
	}
	return added, removed
}

func toChannelConfig(cfg *config.Config, list []string) (*config.ChannelsConfig, error) {
	result := make(config.ChannelsConfig)
	for _, name := range list {
		bc, ok := cfg.Channels[name]
		if !ok || !bc.Enabled {
			continue
		}
		result[name] = bc
	}
	return &result, nil
}
