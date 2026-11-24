-- API key time-window control (docs/design/api-key-time-window-control.md):
-- JSON policy {"tz":"Asia/Shanghai","windows":[{"days":[1,5],"start":"09:00","end":"18:00"}]};
-- null/empty = unrestricted.
ALTER TABLE api_keys ADD COLUMN allowed_time_windows TEXT;
