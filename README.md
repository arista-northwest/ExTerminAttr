# ExTerminAttr

### Build

For EOS make sure to set the GOOS and GOARCH environment variables...

```
GOOS=linux GOARCH=386 go build .
```

### Example

```
sudo ./ExTerminAttr -addr management/localhost:6042 -forward_url http://192.168.56.1:8080 /Smash
```
