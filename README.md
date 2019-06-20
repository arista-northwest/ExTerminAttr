# ExTerminAttr

### Build

For EOS make sure to set the GOOS and GOARCH environment variables...

```
GOOS=linux GOARCH=386 go build .
```

### Example

```
sudo ./exterminattr -addr management/localhost:6042 -ns_name management -forward_url http://192.168.56.1:8080 /Sysdb/cell/1
```
