# go-inno-appws (core AppWebsocket)

Go Library for innovaphone AppWebsocket client ([SDK Documentation](https://sdk.innovaphone.com/16r1/doc/appwebsocket/AppWebsocket.htm)).

Provides authentication, framing, and keepalive functionality for the innovaphone AppWebsocket protocoll.

## Testing

Run unit tests:
```bash
go test ./...
```

Run integration tests against an AP Manager on an innovaphone App Plattform:
```bash
APPWS_INTEGRATION=1 APPWS_MANAGER_URL=<url> APPWS_MANAGER_PASSWORD=<password> go test ./appws -run TestDialManagerIntegration
```

### Environment Variables

- `APPWS_INTEGRATION`: Enable integration tests
- `APPWS_MANAGER_URL`: Manager websocket URL (optional, defaults to test endpoint)
- `APPWS_MANAGER_PASSWORD`: Required password for integration tests
