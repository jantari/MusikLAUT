package main

import (
    "bytes"
    "context"
    "crypto/aes"
    "crypto/cipher"
    "crypto/hmac"
    "crypto/rand"
    "crypto/sha1"
    "encoding/base64"
    "encoding/json/jsontext"
    json "encoding/json/v2"
    "errors"
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "net/url"
    "os"
    "strings"
    "sync"
    "time"

    // Internal imports
    "MusikLAUT/dh"

    // Configuration
    "github.com/peterbourgon/ff/v3"

    // Launching the OS-native browser with the login-link
    "github.com/pkg/browser"

    // For mDNS-based device discovery
    "github.com/grandcat/zeroconf"
)

var scopes = []string{"user-read-playback-state", "user-modify-playback-state"}

var authorizationCode = ""
var accessToken = ""

var TrackIds = map[string]string{
    "Ophelia":     "spotify:track:53iuhJlwXhSER5J2IYYv1W",
    "Koerperteil": "spotify:track:3wECJLFkS6cGvdyVOmGFme",
}

type TokenResponse struct {
    AccessToken  string `json:"access_token"`
    TokenType    string `json:"token_type"`
    ExpiresIn    int    `json:"expires_in"`
    RefreshToken string `json:"refresh_token"`
    Scope        string `json:"scope"`
}

type DevicesResponse struct {
    Devices []SpotifyDevice `json:"devices"`
}

type PlaybackStateResponse struct {
    Device    SpotifyDevice `json:"device"`
    IsPlaying bool          `json:"is_playing"`
    Item      struct {
        Name  string `json:"name"`
        Type  string `json:"type"`
    } `json:"item"`
}

type SpotifyDevice struct {
    Id               string `json:"id"`
    IsActive         bool   `json:"is_active"`
    IsPrivateSession bool   `json:"is_private_session"`
    IsRestricted     bool   `json:"is_restricted"`
    Name             string `json:"name"`
    SupportsVolume   bool   `json:"supports_volume"`
    Type             string `json:"type"`
    VolumePercent    int    `json:"volume_percent"`
}

type mDNSSpotifyDevice struct {
    ZeroConfBaseURL string
    MDNSHostname    string
    IPv4Addresses   []net.IP
    IPv6Addresses   []net.IP

    ZeroConfInfo    mDNSSpotifyDeviceInfo
}

type mDNSSpotifyDeviceInfo struct {
    Availability          string `json:"availability"`
    BrandDisplayName      string `json:"brandDisplayName"`
    ClientId              string `json:"clientID"`
    DeviceId              string `json:"deviceID"`
    DeviceType            string `json:"deviceType"`
    GroupStatus           string `json:"groupStatus"`
    LibraryVersion        string `json:"libraryVersion"`
    ModelDisplayName      string `json:"modelDisplayName"`
    ProductId             int    `json:"productID"`
    PublicKey             string `json:"publicKey"`
    RemoteName            string `json:"remoteName"`
    ResolverVersion       string `json:"resolverVersion"`
    Scope                 string `json:"scope"`
    SpotifyError          int    `json:"spotifyError"`
    Status                int    `json:"status"`
    StatusString          string `json:"statusString"`
    TokenType             string `json:"tokenType"`
    Version               string `json:"version"`
    SupportedCapabilities int    `json:"supported_capabilities"`
}

func PrintJSON(obj interface{}) {
    b, _ := json.Marshal(
        obj,
        json.OmitZeroStructFields(true),
        json.StringifyNumbers(true),
        jsontext.WithIndent("  "),
    )
    fmt.Println(string(b))
}

func randString(nByte int) (string, error) {
    b := make([]byte, nByte)

    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return base64.RawURLEncoding.EncodeToString(b), nil
}

func startCallbackListenerAsync(wg *sync.WaitGroup) *http.Server {
    srv := &http.Server{Addr: ":1235"}

    http.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
        fmt.Printf("Got Spotify auth callback: %v\n", r.URL.Query())
        fmt.Println("Headers:")
        for k, v := range r.Header {
            fmt.Printf("  %v: %v\n", k, v)
        }

        if r.URL.Query().Has("code") {
            authorizationCode = r.URL.Query().Get("code")
            w.Write([]byte("Looks good!"))
        } else {
            w.Write([]byte("Something probably went wrong :("))
        }
    })

    go func() {
        defer wg.Done() // let main know we are done cleaning up

        // always returns error. ErrServerClosed on graceful close
        if err := srv.ListenAndServe(); err != http.ErrServerClosed {
            // unexpected error. port in use?
            log.Fatalf("ListenAndServe(): %v", err)
        }
    }()

    // returning reference so caller can call Shutdown()
    return srv
}

type apResolveResponse struct {
    Accesspoint []string `json:"accesspoint,omitempty"`
    Dealer      []string `json:"dealer,omitempty"`
    Spclient    []string `json:"spclient,omitempty"`
}

func obtainReusableCredentialsBlob() error {
    // Turns out doing this the real way is far too complicated.
    // If I implement this, it's going to be by starting librespot
    // or librespot-go as a separate process and waiting for the
    // credentials blob to show up in their state/config files

    // See librespot-go source for how it would be done (connect to ap, speak protobuf via socket):
    // https://github.com/devgianlu/go-librespot/blob/master/session/session.go#L34
    // https://github.com/devgianlu/go-librespot/blob/master/ap/ap.go#L120
    // https://github.com/devgianlu/go-librespot/blob/master/ap/ap.go#L550
    return nil
}

func mDnsDiscoverDevicesAsync(searchTime time.Duration) []mDNSSpotifyDevice {
    var discoveredDevices []mDNSSpotifyDevice
    fmt.Println("mDNS discovery")
    // Discover all services on the network
    resolver, err := zeroconf.NewResolver(nil)
    if err != nil {
        log.Fatalln("Failed to initialize resolver:", err.Error())
    }

    entries := make(chan *zeroconf.ServiceEntry)
    go func(results <-chan *zeroconf.ServiceEntry) {
        for entry := range results {
            //log.Println(entry)
            var zeroConfPath string
            for _, val := range entry.Text {
                if strings.HasPrefix(val, "CPath=") {
                    zeroConfPath = strings.TrimPrefix(val, "CPath=")
                }
            }
            fmt.Printf("Spotify Connect device: %v, query http://%v:%v%v?action=getInfo\n", entry.Instance, entry.HostName, entry.Port, zeroConfPath)

            req, err := http.NewRequest("GET", fmt.Sprintf("http://%v:%v%v?action=getInfo", entry.HostName, entry.Port, zeroConfPath), nil)
            if err != nil {
                log.Fatal(err)
            }

            client := &http.Client{}
            resp, err := client.Do(req)
            if err != nil {
                log.Fatal(err)
            }
            defer resp.Body.Close()

            fmt.Println("response Status:", resp.Status)
            fmt.Println("response Headers:", resp.Header)
            body, _ := io.ReadAll(resp.Body)
            //fmt.Println("response Body:", string(body))

            var device mDNSSpotifyDevice
            var deviceZC mDNSSpotifyDeviceInfo
            if err := json.Unmarshal(body, &deviceZC); err != nil { // Parse []byte to go struct pointer
                log.Println("mDNS Can not unmarshal zeroconf JSON")
            }

            device.ZeroConfBaseURL = fmt.Sprintf(":%v%v", entry.Port, zeroConfPath)
            device.ZeroConfInfo    = deviceZC
            device.MDNSHostname    = entry.HostName
            device.IPv4Addresses   = entry.AddrIPv4
            device.IPv6Addresses   = entry.AddrIPv6

            discoveredDevices = append(discoveredDevices, device)

            PrintJSON(device)
        }
        log.Println("mDNS No more entries.")
    }(entries)

    ctx, cancel := context.WithTimeout(context.Background(), searchTime)
    defer cancel()
    err = resolver.Browse(ctx, "_spotify-connect._tcp", "local.", entries)
    if err != nil {
        log.Println("mDNS Failed to browse:", err.Error())
    }

    <-ctx.Done()

    return discoveredDevices
}

// To "wake" a device and make it castable again via the WebAPI,
// we have to re-login to it. This happens via us (the casting device)
// sending a special login-blob to the addUser zeroconf API with which
// the casted-to device can then login itself to our Spotify account
func mDNSWakeDevice(device mDNSSpotifyDevice, dh *dh.DiffieHellman, userName string) error {
    devicePublicKey, err := base64.StdEncoding.DecodeString(device.ZeroConfInfo.PublicKey)
    if err != nil {
        log.Fatalf("Failed to decode devices base64 publickey: %v\n", err)
    }
    dh.Exchange(devicePublicKey)
    sharedSecret := dh.SharedSecretBytes()
    fmt.Printf("Did DH exchange, calculated shared secret: %v\n", base64.StdEncoding.EncodeToString(sharedSecret))

    // Begin attempt to construct a valid "blob"
    baseKey := func() []byte { sum := sha1.Sum(sharedSecret); return sum[:16] }()
    mac := hmac.New(sha1.New, baseKey)
    mac.Write([]byte("checksum"))

    checksumKey := mac.Sum(nil)
    fmt.Printf("checksumKey: %v\n", checksumKey)

    mac.Reset()
    mac.Write([]byte("encryption"))
    encryptionKey := func() []byte { sum := mac.Sum(nil); return sum[:16] }()
    fmt.Printf("encryptionKey: %v\n", encryptionKey)

    bc, err := aes.NewCipher(encryptionKey)
    if err != nil {
        log.Fatalf("failed initializing aes cihper: %w", err)
    }

    // Generate a random IV to use for encryption
    iv := make([]byte, 16)
    if _, err = rand.Read(iv); err != nil {
        log.Fatalf("failed reading random data for IV: %w", err)
    }

    // Test payload, was successfully reconstructed (decrypted and verified) on the other end by librespot-go
    //payload := []byte{1,2,3,4,5}
    // I don't know yet what the unencrypted raw payload really has to be,
    // looking into the callstack in go-librespot it gets passed around a bit
    // and then base64-decoded and processed futher in: https://github.com/devgianlu/go-librespot/blob/master/ap/ap.go#L136
    // This means it (the payload) for sure has to be base64-encoded bytes
    // let's just try sending an access token lul
    payload := make([]byte, base64.StdEncoding.EncodedLen(len([]byte(authorizationCode))))
    base64.StdEncoding.Encode(payload, []byte(authorizationCode))

    encrypted := make([]byte, len(payload))
    cipher.NewCTR(bc, iv).XORKeyStream(encrypted, payload)

    // Now we have:
    // - iv
    // - encrypted
    // time to calculate the checksum:

    mac = hmac.New(sha1.New, checksumKey)
    mac.Write(encrypted)
    checksum := mac.Sum(nil)

    blobStr := base64.StdEncoding.EncodeToString(append(iv, append(encrypted, checksum...)...))

    fmt.Printf("BLOB IS READY:\n")
    fmt.Printf(" iv:       %v\n", iv)
    fmt.Printf(" payload:  %v\n", encrypted)
    fmt.Printf(" checksum: %v\n", checksum)
    fmt.Printf("BLOBSTR:   %v\n", blobStr)

    data := url.Values{}
    data.Set("action", "addUser")
    data.Set("userName", *userIdPtr)
    data.Set("tokenType", "default")
    data.Set("clientKey", base64.StdEncoding.EncodeToString(dh.PublicKeyBytes()))
    data.Set("deviceName", "MusikLAUT")
    data.Set("version", "2.12.0")
    data.Set("blob", blobStr)

    req, err := http.NewRequest("POST", fmt.Sprintf("http://%v%v", device.MDNSHostname, device.ZeroConfBaseURL), strings.NewReader(data.Encode()))
    if err != nil {
        log.Fatal(err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    fmt.Println("Done!")

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, _ := io.ReadAll(resp.Body)
    fmt.Println("response Body:", string(body))

    return nil
}

func startAuthorizeFlow(clientId string) {
    state, err := randString(16)
    if err != nil {
        log.Fatal(err)
    }

    req, err := http.NewRequest("GET", "https://accounts.spotify.com/authorize", nil)
    if err != nil {
        log.Fatal(err)
    }

    q := req.URL.Query()
    q.Add("response_type", "code")
    q.Add("client_id", clientId)
    q.Add("scope", strings.Join(scopes, " "))
    q.Add("redirect_uri", "http://127.0.0.1:1235/redirect")
    q.Add("state", state)

    req.URL.RawQuery = q.Encode()

    fmt.Println(req.URL.String())

    // Open the browser for user-interactive authorization
    browser.OpenURL(req.URL.String())
}

func play(trackid string, deviceid string) (err error) {
    var jsonString = fmt.Sprintf(`{"uris":["%v"],"position_ms":0}`, trackid)
    var jsonBody = []byte(jsonString)

    req, err := http.NewRequest("PUT", "https://api.spotify.com/v1/me/player/play", bytes.NewReader(jsonBody))
    q := req.URL.Query()
    q.Set("device_id", deviceid)
    req.URL.RawQuery = q.Encode()

    req.Header.Set("Authorization", fmt.Sprintf("Bearer %v", accessToken))
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, _ := io.ReadAll(resp.Body)
    fmt.Println("response Body:", string(body))

    return nil
}

func main() {
    fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
    var userIdPtr       = fs.String("userId",        "", "The Spotify user accounts ID")
    var clientIdPtr     = fs.String("clientId",      "", "The Spotify application API Token Client-ID")
    var clientSecretPtr = fs.String("clientSecret",  "", "The Spotify application API Token Client-Secret")
    var trackIdPtr      = fs.String("trackId",       "", "The Spotify trackid to play")
    var deviceNamePtr   = fs.String("deviceName",    "", "The name of the device to play it on")

    // Ingest configuration flags.
    // Commandline arguments > Environment variables
    err := ff.Parse(
        fs,
        os.Args[1:],
        ff.WithEnvVarPrefix("MUSIKLAUT"),
    )
    if err != nil {
        // Replicate default ExitOnError behavior of exiting with 0 when -h/-help/--help is used
        if errors.Is(err, flag.ErrHelp) {
            os.Exit(0)
        }
        fmt.Println(err)
        os.Exit(2)
    }

    if *clientIdPtr == "" || *clientSecretPtr == "" {
        log.Fatal("clientId and clientSecret are required")
    }

    fmt.Println("Starting")
    dh, err := dh.NewDiffieHellman()
    if err != nil {
        log.Fatalf("failed initializing diffiehellman: %w", err)
    }
    fmt.Printf("Generated DH public key: %v\n", base64.StdEncoding.EncodeToString(dh.PublicKeyBytes()))

    mDNSDevices := mDnsDiscoverDevicesAsync(5 * time.Second)

    fmt.Println("Login & API-based connections")

    httpServerExitDone := &sync.WaitGroup{}
    httpServerExitDone.Add(1)

    srv := startCallbackListenerAsync(httpServerExitDone)

    startAuthorizeFlow(*clientIdPtr)

    // TODO don't just wait a random time, kill the server + continue when the URL handler is called
    fmt.Println("Waiting for callback ...")
    time.Sleep(8 * time.Second)

    if err := srv.Shutdown(context.TODO()); err != nil {
        panic(err) // failure/timeout shutting down the server gracefully
    }

    fmt.Println("Done!")
    fmt.Println(authorizationCode)

    data := url.Values{}
    data.Set("grant_type", "authorization_code")
    data.Set("code", authorizationCode)
    data.Set("redirect_uri", "http://127.0.0.1:1235/redirect")

    req, err := http.NewRequest("POST", "https://accounts.spotify.com/api/token", strings.NewReader(data.Encode()))
    if err != nil {
        log.Fatal(err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    req.SetBasicAuth(*clientIdPtr, *clientSecretPtr)

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    fmt.Println("Done!")

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, _ := io.ReadAll(resp.Body)
    fmt.Println("response Body:", string(body))

    // Mit dem access_token aus der response kann man dann API requests machen, z.B.:
    // curl --request GET --url https://api.spotify.com/v1/me/player/devices --header "Authorization: Bearer $TOKEN"

    var result TokenResponse
    if err := json.Unmarshal(body, &result); err != nil { // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    fmt.Printf("Got access token: %v\n", result.AccessToken)
    accessToken = result.AccessToken

    // Make first relevant Spotify API call - GET AVAILABLE PLAYBACK DEVICES (CONNECT, CAST-TO)
    req, err = http.NewRequest("GET", "https://api.spotify.com/v1/me/player/devices", nil)
    req.Header.Set("Authorization", fmt.Sprintf("Bearer %v", result.AccessToken))
    client = &http.Client{}
    resp, err = client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, _ = io.ReadAll(resp.Body)
    fmt.Println("response Body:", string(body))

    var resultDevices DevicesResponse
    if err := json.Unmarshal(body, &resultDevices); err != nil { // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    fmt.Printf("%+v\n", resultDevices.Devices)

    var deviceId string
    for _, device := range resultDevices.Devices {
        if device.Name == *deviceNamePtr {
            deviceId = device.Id
            break
        }
    }
    if deviceId == "" {
        log.Printf("Spotify device with the name '%v' could not be found (offline?)\n", *deviceNamePtr)

        for _, device := range mDNSDevices {
            if *deviceNamePtr == device.ZeroConfInfo.RemoteName {
                fmt.Printf("Spotify device '%v' was found via mDNS/zeroconf though, attempting to zeroconf wake it\n", *deviceNamePtr)
                mDNSWakeDevice(device, dh, *userIdPtr)
            }
        }
    }

    // Get current playback status (e.g. currently active device)
    // https://api.spotify.com/v1/me/player
    req, err = http.NewRequest("GET", "https://api.spotify.com/v1/me/player", nil)
    req.Header.Set("Authorization", fmt.Sprintf("Bearer %v", result.AccessToken))
    client = &http.Client{}
    resp, err = client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, _ = io.ReadAll(resp.Body)
    fmt.Println("response Body:", string(body))

    var resultPlayback PlaybackStateResponse
    if err := json.Unmarshal(body, &resultPlayback); err != nil { // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    if resultPlayback.IsPlaying {
        fmt.Printf("Currently playing %v '%v' on '%v'\n", resultPlayback.Item.Type, resultPlayback.Item.Name, resultPlayback.Device.Name)
    } else {
        fmt.Println("Currently not playing")
    }

    time.Sleep(1 * time.Second)

    fmt.Println("Switching playback...")
    play(*trackIdPtr, deviceId)
}

