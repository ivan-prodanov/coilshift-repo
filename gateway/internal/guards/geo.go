package guards

import "math"

const earthRadiusM = 6371000.0

// LatLon is a WGS84 position in degrees.
type LatLon struct{ Lat, Lon float64 }

// DistanceM is the haversine distance in metres.
func DistanceM(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	p1, p2 := lat1*rad, lat2*rad
	dp, dl := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a)))
}

// ValidLatLon reports whether the pair is finite, in range and not the
// null-island 0/0 placeholder.
func ValidLatLon(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return false
	}
	if math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return false
	}
	return !(lat == 0 && lon == 0)
}
