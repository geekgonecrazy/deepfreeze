package main

import "testing"

func TestUriWithoutDatabase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "uri with database and query gets authSource added",
			in:   "mongodb://user:pw@mongo-1,mongo-2:27017/rocket?replicaSet=rs01",
			want: "mongodb://user:pw@mongo-1,mongo-2:27017/?replicaSet=rs01&authSource=rocket",
		},
		{
			name: "existing authSource is preserved",
			in:   "mongodb://user:pw@mongo-1:27017/appdata?replicaSet=rs01&authSource=admin",
			want: "mongodb://user:pw@mongo-1:27017/?replicaSet=rs01&authSource=admin",
		},
		{
			name: "database without query",
			in:   "mongodb://mongo-1:27017/appdata",
			want: "mongodb://mongo-1:27017/?authSource=appdata",
		},
		{
			name: "already database-less URI is untouched",
			in:   "mongodb://mongo-1:27017?replicaSet=rs01",
			want: "mongodb://mongo-1:27017?replicaSet=rs01",
		},
		{
			name: "empty database path with query is untouched",
			in:   "mongodb://mongo-1:27017/?replicaSet=rs01",
			want: "mongodb://mongo-1:27017/?replicaSet=rs01",
		},
		{
			name: "uri without slash or query is untouched",
			in:   "mongodb://mongo-1:27017",
			want: "mongodb://mongo-1:27017",
		},
		{
			name: "srv scheme works",
			in:   "mongodb+srv://cluster.example.com/appdata",
			want: "mongodb+srv://cluster.example.com/?authSource=appdata",
		},
		{
			name: "url-encoded slash in password does not confuse parsing",
			in:   "mongodb://user:p%2Fw@mongo-1:27017/appdata",
			want: "mongodb://user:p%2Fw@mongo-1:27017/?authSource=appdata",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uriWithoutDatabase(tc.in); got != tc.want {
				t.Errorf("uriWithoutDatabase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
