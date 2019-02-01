package servermanager

// ConfigIniDefault is the default server config (ish) as supplied via the assetto corsa server.
var ConfigIniDefault = ServerConfig{
	Server: ServerSetupConfig{
		GlobalServerConfig: GlobalServerConfig{
			Name:                      "Server Managed Assetto Corsa",
			Password:                  "",
			AdminPassword:             "",
			UDPPort:                   9600,
			TCPPort:                   9600,
			HTTPPort:                  8081,
			ClientSendIntervalInHertz: 18,
			SendBufferSize:            0,
			ReceiveBufferSize:         0,
			KickQuorum:                85,
			VotingQuorum:              80,
			VoteDuration:              20,
			BlacklistMode:             1,
			RegisterToLobby:           0, // @TODO UNDOME
			MaxClients:                18,
			UDPPluginLocalPort:        0,
			UDPPluginAddress:          "",
			AuthPluginAddress:         "",
			NumThreads:                2,
			ResultScreenTime:          10,
		},

		CurrentRaceConfig: CurrentRaceConfig{
			Cars:                      "bmw_m3_e30",
			TrackLayout:               "",
			Track:                     "magione",
			SunAngle:                  48,
			PickupModeEnabled:         1,
			LoopMode:                  1,
			SleepTime:                 1,
			RaceOverTime:              180,
			FuelRate:                  100,
			DamageMultiplier:          100,
			TyreWearRate:              100,
			AllowedTyresOut:           2,
			ABSAllowed:                1,
			TractionControlAllowed:    1,
			StabilityControlAllowed:   0,
			AutoClutchAllowed:         0,
			TyreBlanketsAllowed:       1,
			ForceVirtualMirror:        1,
			LegalTyres:                "SV",
			LockedEntryList:           0,
			RacePitWindowStart:        25,
			RacePitWindowEnd:          35,
			ReversedGridRacePositions: 8,
			TimeOfDayMultiplier:       1,
			QualifyMaxWaitPercentage:  120,
			RaceGasPenaltyDisabled:    0,
			MaxBallastKilograms:       50,
			WindBaseSpeedMin:          3,
			WindBaseSpeedMax:          15,
			WindBaseDirection:         30,
			WindVariationDirection:    15,
			StartRule:                 0,
		},
	},

	Sessions: map[SessionType]SessionConfig{
		SessionTypePractice: {
			Name:   "Practice",
			Time:   10,
			IsOpen: 1,
		},
		SessionTypeQualifying: {
			Name:   "Qualify",
			Time:   10,
			IsOpen: 1,
		},
		SessionTypeRace: {
			Name:     "Race",
			IsOpen:   1,
			WaitTime: 60,
			Laps:     5,
		},
	},

	DynamicTrack: DynamicTrackConfig{
		SessionStart:    89,
		Randomness:      3,
		SessionTransfer: 80,
		LapGain:         50,
	},

	Weather: map[string]WeatherConfig{
		"WEATHER_0": {
			Graphics:               "3_clear",
			BaseTemperatureAmbient: 18,
			BaseTemperatureRoad:    6,
			VariationAmbient:       1,
			VariationRoad:          1,
		},
		"WEATHER_1": {
			Graphics:               "7_heavy_clouds",
			BaseTemperatureAmbient: 15,
			BaseTemperatureRoad:    -1,
			VariationAmbient:       1,
			VariationRoad:          1,
		},
	},
}
