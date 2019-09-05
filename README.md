# ExTerminAttr

### Build

For EOS make sure to set the GOOS and GOARCH environment variables...

```
GOOS=linux GOARCH=386 go build .
```

### Examples


#### On switch...

```
bash /mnt/flash/ExTerminAttr -forward_url http://192.168.56.1:8080 /Smash
```

#### In a VRF

```
bash sudo ip netns exec ns-management /mnt/flash/ExTerminAttr -forward_url http://192.168.56.1:8080 /Smash
```

#### As a daemon

```


```
sudo ./ExTerminAttr -forward_url http://192.168.56.1:8080 /Smash
```

#### Off switch...

```
ExTerminAttr -addr <switch-ip>:6042 -forward_url http://localhost:8080 -paths_file paths.cfg
```
paths.cfg file MUST be space delimited.
