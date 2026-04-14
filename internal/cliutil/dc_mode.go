package cliutil

import "context"

type DataChannelModeRunner func(context.Context, string, string) error

func RunSelectedDataChannelMode(
	ctx context.Context,
	yalink string,
	jazzRoom string,
	targetAddr string,
	vlessMode bool,
	runTelemost DataChannelModeRunner,
	runTelemostVLESS DataChannelModeRunner,
	runJazz DataChannelModeRunner,
	runJazzVLESS DataChannelModeRunner,
) (string, error) {
	if yalink != "" {
		if vlessMode {
			return "Telemost", runTelemostVLESS(ctx, yalink, targetAddr)
		}
		return "Telemost", runTelemost(ctx, yalink, targetAddr)
	}
	if jazzRoom != "" {
		if vlessMode {
			return "SaluteJazz", runJazzVLESS(ctx, jazzRoom, targetAddr)
		}
		return "SaluteJazz", runJazz(ctx, jazzRoom, targetAddr)
	}
	return "", nil
}
