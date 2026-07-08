package main

import (
    "os"
	"bytes"
	"context"
	"crypto/rand"
    "flag"
    "errors"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

    // Configuration
    "github.com/peterbourgon/ff/v3"

    // Launching the OS-native browser with the login-link
	"github.com/pkg/browser"

    // For mDNS-based device discovery
    "github.com/grandcat/zeroconf"
)

var scopes = []string { "user-read-playback-state", "user-modify-playback-state" }

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

func randString(nByte int) (string, error) {
    b := make([]byte, nByte)
    if _, err := io.ReadFull(rand.Reader, b); err != nil {
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

    fmt.Println("Starting")

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
            for _, val := range(entry.Text) {
                if strings.HasPrefix(val, "CPath=") {
                    zeroConfPath = strings.TrimPrefix(val, "CPath=")
                }
            }
            fmt.Printf("Spotify Connect device: %v, query http://%v:%v%v?action=getInfo\n", entry.Instance, entry.HostName, entry.Port, zeroConfPath)
            fmt.Printf("  IPs: %v %v\n", entry.AddrIPv4, entry.AddrIPv6)
        }
        log.Println("No more entries.")
    }(entries)

    ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
    defer cancel()
    err = resolver.Browse(ctx, "_spotify-connect._tcp", "local.", entries)
    if err != nil {
        log.Fatalln("Failed to browse:", err.Error())
    }

    <-ctx.Done()

    fmt.Println("Login & API-based connections")

    httpServerExitDone := &sync.WaitGroup{}
    httpServerExitDone.Add(1)

    srv := startCallbackListenerAsync(httpServerExitDone)

    startAuthorizeFlow(*clientIdPtr)

    // TODO don't just wait a random time, kill the server + continue when the URL handler is called
    fmt.Println("Waiting for callback ...")
    time.Sleep(1 * time.Second)

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
    if err := json.Unmarshal(body, &result); err != nil {   // Parse []byte to go struct pointer
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
    if err := json.Unmarshal(body, &resultDevices); err != nil {   // Parse []byte to go struct pointer
        fmt.Println("Can not unmarshal JSON")
    }

    fmt.Printf("%+v\n", resultDevices.Devices)

    var deviceId string
    for _, device := range(resultDevices.Devices) {
        if device.Name == *deviceNamePtr {
            deviceId = device.Id
            break
        }
    }
    if deviceId == "" {
        log.Fatalf("Spotify device with the name '%v' could not be found (offline?)", *deviceNamePtr)
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
    if err := json.Unmarshal(body, &resultPlayback); err != nil {   // Parse []byte to go struct pointer
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

