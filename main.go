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
	"encoding/binary"
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
	"golang.org/x/term"

	"golang.org/x/crypto/pbkdf2"

	// Internal imports
	"MusikLAUT/dh"

	// Configuration
	"github.com/peterbourgon/ff/v3"

	// Launching the OS-native browser with the login-link
	"github.com/pkg/browser"

	// For mDNS-based device discovery
	"github.com/grandcat/zeroconf"
)

var scopes = []string{"user-read-playback-state", "user-modify-playback-state", "streaming"} // Not sure at all if "streaming" is required

var authorizationCode = ""
var accessToken = ""
var dhState *dh.DiffieHellman

var TrackIds = map[string]string{
    "Ophelia":     "spotify:track:53iuhJlwXhSER5J2IYYv1W",
    "Opalite":     "spotify:track:3yWuTOYDztXjZxdE2cIRUa",
    "Koerperteil": "spotify:track:3wECJLFkS6cGvdyVOmGFme",
    "GuteLaune":   "spotify:track:7fapAlfgJf6EzlviBqJb4f",
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

type SpotifyOAuth struct {
    CallbackEventCh chan string
}

func (h *SpotifyOAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    fmt.Printf("Got Spotify auth callback: %v\n", r.URL.Query())
    fmt.Println("Headers:")
    for k, v := range r.Header {
        fmt.Printf("  %v: %v\n", k, v)
    }

    if r.URL.Query().Has("code") {
        h.CallbackEventCh <- r.URL.Query().Get("code")
        w.Write([]byte("Looks good!"))
    } else {
        http.Error(w, "Something went wrong, no 'code' in URL", http.StatusExpectationFailed)
    }
}


func startCallbackListenerAsync(wg *sync.WaitGroup, authURL string, ch chan string) *http.Server {
    srv := &http.Server{Addr: ":1235"}

    redirectHandler := &SpotifyOAuth{CallbackEventCh: ch}
    http.Handle("GET /redirect", redirectHandler)

    http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
        http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
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
            fmt.Printf("Spotify Connect device: %v, query http://%v:%v%v?action=getInfo\n", entry.Instance, entry.AddrIPv4[0], entry.Port, zeroConfPath)

            fmt.Printf("ALL-DATA: %+v\n", entry)

            req, err := http.NewRequest("GET", fmt.Sprintf("http://%v:%v%v?action=getInfo", entry.AddrIPv4[0], entry.Port, zeroConfPath), nil)
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

func buildZeroconfAuthBlob(deviceId string, devicePublicKeyB64 string, userName string, authBlobFile string) string {
    // I don't know yet what the "authData" has to be, but it kind of looks like an authentication_code / OAuth token.
    // For testing purposes we can just paste in an old authData block that was sent by a Spotify client previously and packet-captured, they are replayable
    b64AuthData, err := os.ReadFile(authBlobFile)
    if err != nil {
        log.Fatalf("buildZeroconfAuthBlob(): could not read authBlob file %v", err)
    }
    authData := make([]byte, base64.StdEncoding.EncodedLen(len(b64AuthData)))
    base64.StdEncoding.Encode(authData, b64AuthData)
    if err != nil {
        log.Fatalf("buildZeroconfAuthBlob(): could not decode authBlob base64 %v", err)
    }

    // Begin constructing main payload
    var payload []byte
    // First byte is discarded, make it anything (my phone sent 73 to go-librespot so let's use that)
    payload = append(payload, 73)
    // Write a uint64 to the payload, it specifies how many following bytes to discard (at least to go-librespot)
    payload = binary.AppendUvarint(payload, 28)
    // Add the 28 bytes to be skipped, go-librespot and my Yamaha AVR don't seem to care if this is random/bogus data
    payload = append(payload, []byte{42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42, 42}...)
    // Another byte that is discarded
    payload = append(payload, 80)
    // Write the authenticationType (uint64)
    payload = binary.AppendUvarint(payload, 1) // 1 == "AUTHENTICATION_STORED_SPOTIFY_CREDENTIALS"
    // Another byte that is discarded
    payload = append(payload, 81)
    // Write a uint64 to the payload, it specifies the length of the authData block after it.
    // again, having issues with not understanding AppendUvarint, do it with append for now
    payload = binary.AppendUvarint(payload, uint64(len(authData)))
    // Next is the actual authData/stored-credentials block
    payload = append(payload, []byte(authData)...)

    fmt.Printf("raw unencrypted blob payload (pre-encryption, pre-XOR, unpadded): %v\n", payload)

    // Encrypt payload with deviceId + username (the first, inner encryption)
    secret := sha1.Sum([]byte(deviceId))
    baseKey := pbkdf2.Key(secret[:], []byte(userName), 256, 20, sha1.New)
    key := make([]byte, 24)
    copy(key, func() []byte { sum := sha1.Sum(baseKey); return sum[:] }())
    binary.BigEndian.PutUint32(key[20:], 20)

    fmt.Printf("AES initialization key derived from target deviceId '%v': %v\n", deviceId, key)

    devidusrnameBlockCipher, err := aes.NewCipher(key)
    if err != nil {
        fmt.Println("failed initializing AES cipher to encrypt blob payload with targets deviceId:", err)
    }

    fmt.Printf("About to encrypt blob payload with key derived from deviceId\n")

    // Pad our B64-encoded payload for AES block cipher (pkcs7 standard)
    n := aes.BlockSize - (len(payload) % aes.BlockSize)
    payloadPadded := make([]byte, len(payload)+n)
    copy(payloadPadded, payload)
    copy(payloadPadded[len(payload):], bytes.Repeat([]byte{byte(n)}, n))

    // Weird offset XOR obfuscation
    for j := 16; j < len(payloadPadded); j++ {
        payloadPadded[j] ^= payloadPadded[j-16]
    }
    // The above is the copilot-suggested inverse of the below original XOR that's applied
    // in go-librespot (the cast-to device). EDIT: it worked first try, I am shook
    //for i := 0; i < len(payloadPadded)-16; i++ {
    //    payloadPadded[len(payloadPadded)-i-1] ^= payloadPadded[len(payloadPadded)-i-17]
    //}

    fmt.Printf("raw unencrypted blob payload (pre-encryption, XORd, padded): %v\n", payloadPadded)

    encryptedPayload := make([]byte, len(payloadPadded))
    for i := 0; i < len(payloadPadded)-1; i += aes.BlockSize {
        devidusrnameBlockCipher.Encrypt(encryptedPayload[i:], payloadPadded[i:])
    }

    fmt.Printf("raw once-encrypted blob payload: %v\n", encryptedPayload)

    // Base64 encode inner encrypted payload
    payloadB64 := make([]byte, base64.StdEncoding.EncodedLen(len(encryptedPayload)))
    base64.StdEncoding.Encode(payloadB64, encryptedPayload)

    // At this point, the central payload (authData) is encrypted using the
    // users userName and the target devices deviceId and then base64-encoded
    //
    // The resulting base64 bytes are now encrypted again with a shared secret
    // obtained through a DH exchange (and then once more base64-encoded)

    // Do DH exchange / calculation
    devicePublicKey, err := base64.StdEncoding.DecodeString(devicePublicKeyB64)
    if err != nil {
        log.Fatalf("Failed to decode devices base64 publickey: %v\n", err)
    }
    dhState.Exchange(devicePublicKey)
    sharedSecret := dhState.SharedSecretBytes()
    fmt.Printf("Did DH exchange, calculated shared secret: %v\n", base64.StdEncoding.EncodeToString(sharedSecret))

    baseKey = func() []byte { sum := sha1.Sum(sharedSecret); return sum[:16] }()
    mac := hmac.New(sha1.New, baseKey)
    mac.Write([]byte("checksum"))

    checksumKey := mac.Sum(nil)
    fmt.Printf("checksumKey: %v\n", checksumKey)

    mac.Reset()
    mac.Write([]byte("encryption"))
    encryptionKey := func() []byte { sum := mac.Sum(nil); return sum[:16] }()
    fmt.Printf("encryptionKey: %v\n", encryptionKey)

    dhexchangeCipher, err := aes.NewCipher(encryptionKey)
    if err != nil {
        log.Fatalf("failed initializing aes cihper: %w", err)
    }

    // Generate a random IV to use for outer encryption (with DH secret)
    iv := make([]byte, 16)
    if _, err = rand.Read(iv); err != nil {
        log.Fatalf("failed reading random data for IV: %w", err)
    }

    // Perform outer encryption with DH secret key
    dhEncrypted := make([]byte, len(payloadB64))
    cipher.NewCTR(dhexchangeCipher, iv).XORKeyStream(dhEncrypted, payloadB64)

    // Now we have:
    // - iv
    // - twice-encrypted payload
    // time to calculate the checksum:

    mac = hmac.New(sha1.New, checksumKey)
    mac.Write(dhEncrypted)
    checksum := mac.Sum(nil)

    // Join [iv + payload + checksum] and base64-encode everything, that's the final blob
    blobStr := base64.StdEncoding.EncodeToString(append(iv, append(dhEncrypted, checksum...)...))

    fmt.Printf("BLOB IS READY:\n")
    fmt.Printf(" iv:       %v\n", iv)
    fmt.Printf(" payload:  %v\n", dhEncrypted)
    fmt.Printf(" checksum: %v\n", checksum)
    fmt.Printf("BLOBSTR:   %v\n", blobStr)

    return blobStr
}

// To "wake" a device and make it castable again via the WebAPI,
// we have to re-login to it. This happens via us (the casting device)
// sending a special login-blob to the addUser zeroconf API with which
// the casted-to device can then login itself to our Spotify account
func mDNSWakeDevice(device mDNSSpotifyDevice, userName string, authBlobFile string) error {
    blobStr := buildZeroconfAuthBlob(device.ZeroConfInfo.DeviceId, device.ZeroConfInfo.PublicKey, userName, authBlobFile)

    data := url.Values{}
    data.Set("action", "addUser")
    data.Set("userName", userName)
    data.Set("tokenType", "default")
    data.Set("clientKey", base64.StdEncoding.EncodeToString(dhState.PublicKeyBytes()))
    data.Set("deviceName", "MusikLAUT")
    data.Set("version", "2.12.0")
    data.Set("blob", blobStr)

    req, err := http.NewRequest("POST", fmt.Sprintf("http://%v%v", device.IPv4Addresses[0], device.ZeroConfBaseURL), strings.NewReader(data.Encode()))
    if err != nil {
        return fmt.Errorf("could not wake device, preparing addUser request failed: %v", err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf("could not wake device, addUser request failed: %v", err)
    }
    defer resp.Body.Close()

    fmt.Println("addUser response Status:", resp.Status)
    fmt.Println("addUser response Headers:", resp.Header)
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return fmt.Errorf("could not wake device, reading addUser response failed: %v", err)
    }
    fmt.Println("addUser response Body:", string(body))

    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        return nil
    } else {
        return fmt.Errorf("could not wake device, unexpected addUser return code: %v", resp.Status)
    }
}

func startAuthorizeFlow(clientId string) string {
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

    return req.URL.String()
}

func play(trackid string, deviceid string) (err error) {
    var jsonString = fmt.Sprintf(`{"uris":["%v"],"position_ms":0}`, trackid)
    var jsonBody = []byte(jsonString)

    log.Printf("Making API call to play '%v' on '%v'...\n", trackid, deviceid)
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
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return fmt.Errorf("could not play song, reading API response failed: %v", err)
    }
    fmt.Println("response Body:", string(body))

    return nil
}

func getDeviceIdFromName(userId string, deviceName string, apiAccessToken string, authBlobFile string) (string, error) {
    // Make first relevant Spotify API call - GET AVAILABLE PLAYBACK DEVICES (CONNECT, CAST-TO)
    req, err := http.NewRequest("GET", "https://api.spotify.com/v1/me/player/devices", nil)
    req.Header.Set("Authorization", fmt.Sprintf("Bearer %v", apiAccessToken))
    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    fmt.Println("response Status:", resp.Status)
    fmt.Println("response Headers:", resp.Header)
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return "", fmt.Errorf("could not get devices, reading API response failed: %v", err)
    }
    fmt.Println("response Body:", string(body))

    var resultDevices DevicesResponse
    if err := json.Unmarshal(body, &resultDevices); err != nil { // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    fmt.Printf("%+v\n", resultDevices.Devices)

    var deviceId string
    for _, device := range resultDevices.Devices {
        if device.Name == deviceName {
            deviceId = device.Id
            break
        }
    }
    if deviceId == "" {
        log.Printf("Spotify device with the name '%v' could not be found (offline?)\n", deviceName)
        log.Println("Attempting to find sleeping/logged-out devices via mDNS/DNS-SD/zeroconf...")
        mDNSDevices := mDnsDiscoverDevicesAsync(5 * time.Second)

        for _, device := range mDNSDevices {
            if deviceName == device.ZeroConfInfo.RemoteName {
                log.Printf("Spotify device '%v' was found via mDNS/zeroconf though, attempting to zeroconf wake it up\n", deviceName)
                err = mDNSWakeDevice(device, userId, authBlobFile)
                if err != nil {
                    return "", fmt.Errorf("mDNSWakeDevice() failed: %v", err)
                }
                return device.ZeroConfInfo.DeviceId, nil
            }
        }

        return "", fmt.Errorf("could not find any device with the name '%v'", deviceName)
    } else {
        return deviceId, nil
    }
}

func main() {
    fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
    var userIdPtr       = fs.String("userId",       "", "The Spotify user accounts ID")
    var clientIdPtr     = fs.String("clientId",     "", "The Spotify application API Token Client-ID")
    var clientSecretPtr = fs.String("clientSecret", "", "The Spotify application API Token Client-Secret")
    var deviceNamePtr   = fs.String("deviceName",   "", "The name of the device to play it on")
    var authBlobFilePtr = fs.String("authBlobFile", "./authData.txt", "A file with previously captured, base64-encoded 'authData'")

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
    dhState, err = dh.NewDiffieHellman()
    if err != nil {
        log.Fatalf("failed initializing diffiehellman: %w", err)
    }
    fmt.Printf("Generated DH public key: %v\n", base64.StdEncoding.EncodeToString(dhState.PublicKeyBytes()))

    fmt.Println("Login & API-based connections")

    httpServerExitDone := &sync.WaitGroup{}
    httpServerExitDone.Add(1)

    callbackChan := make(chan string)
    authURL := startAuthorizeFlow(*clientIdPtr)
    srv := startCallbackListenerAsync(httpServerExitDone, authURL, callbackChan)

    // Open the browser for user-interactive authorization
    fmt.Println("If a browser does not open automatically, visit the above URL *or* this computer on HTTP port :1235/ (which serves a redirect)")
    browser.OpenURL(authURL)

    // Wait for a callback - this channel read will block
    fmt.Println("Waiting for callback ...")
    authorizationCode, ok := <- callbackChan
    if !ok {
        log.Fatal("callback channel was unexpectedly closed")
    }

    close(callbackChan)
    if err := srv.Shutdown(context.TODO()); err != nil {
        log.Fatalf("Could not stop callback listener: %v", err) // failure/timeout shutting down the server gracefully
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
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        log.Fatalf("could not obtain access token, reading API response failed: %v", err)
    }
    fmt.Println("response Body:", string(body))

    // Mit dem access_token aus der response kann man dann API requests machen, z.B.:
    // curl --request GET --url https://api.spotify.com/v1/me/player/devices --header "Authorization: Bearer $TOKEN"

    var result TokenResponse
    if err := json.Unmarshal(body, &result); err != nil { // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    fmt.Printf("Got access token: %v\n", result.AccessToken)
    accessToken = result.AccessToken

    // GET DEVICEID FROM NAME HERE
    deviceId, err := getDeviceIdFromName(*userIdPtr, *deviceNamePtr, result.AccessToken, *authBlobFilePtr)
    if err != nil {
        log.Fatalf("getDeviceIdFromName() failed: %v", err)
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

    // Ready for input loop
    //
    // switch stdin into 'raw' mode to be able to read single keypresses (like C# Console.ReadKey())
    keypressChannel := make(chan string)

    oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
    if err != nil {
        fmt.Println(err)
        return
    }
    defer term.Restore(int(os.Stdin.Fd()), oldState)

    // Collect and push keypresses to a channel
    go func(ch chan string) {
        var b []byte = make([]byte, 1)
        for {
            os.Stdin.Read(b)
            keypressChannel <- string(b)
        }
    }(keypressChannel)

    // Endlessly process the keypresses from the channel
    fmt.Println("Awaiting input(s)...")
    input:
    for {
        select {
            case stdin, _ := <-keypressChannel:
                fmt.Printf("Key pressed: '%v' (string->[]byte)\n", []byte(stdin))
                switch stdin {
                    case "z":
                        play(TrackIds["Opalite"], deviceId)
                    case "x":
                        play(TrackIds["Ophelia"], deviceId)
                    case "c":
                        play(TrackIds["Koerperteil"], deviceId)
                    case "v":
                        play(TrackIds["GuteLaune"], deviceId)

                    case "\x1b":
                        fmt.Printf("pressed ESC\n")
                        break input
                }
            default:
        }
        time.Sleep(time.Millisecond * 150)
    }
}

