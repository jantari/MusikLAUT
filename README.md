### Usage

```
export MUSIKLAUT_CLIENTID=abc
export MUSIKLAUT_CLIENTSECRET=def

./MusikLAUT -deviceName "Büro" -trackId "spotify:track:53iuhJlwXhSER5J2IYYv1W" # Ophelia
```

---

Making a "disconnected" spotify connect device available again (login/addUser call):

1. discover via mDNS
2. parse out CPath
3. call getInfo
4. call addUser (how?)
  www-url-encoded POST call to root of CPath with payload:
  REQUIRED:
    - action=addUser
    - userName="??" (bspw. von GET https://api.spotify.com/v1/me -> "id" Feld)
    - blob="??" (ist base64URL encoded, ca. 190 bytes lang, maximal 2047 nach docs: https://developer.spotify.com/documentation/commercial-hardware/implementation/reference/3.194#sp_max_zeroconf_blob_length)
    - clientKey="" # can be empty
  OPTIONAL:
    - deviceName= friendly name of connecting spotify connect device/player
    - deviceId= DeviceID of connecting spotify connect device/player
    - loginId= some GUID without the dashes (32 chars)
    - tokenType= "accesstoken" or "authorization_code"
    - version= habe alles von "2.9.0" bis "2.12.0" gesehen, nicht so kritisch
  curl -v -X POST http://mdnsname.local.:80/spotify -d ''
5. device should become castable in WebAPI


MORE NOTES:

- Device passwords: https://www.spotify.com/de/account/set-device-password/  -- useful/required?

- Device Authorization login flow: https://community.spotify.com/t5/Spotify-for-Developers/Device-Authorization-Grant-authentication-flow-for-custom/td-p/5485468

