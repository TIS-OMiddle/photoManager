./photomanager.exe -src ".\images" -dst ".\imagesSorted" -mode "dry-run" -template "{yyyy}/{MM}" -time-source "exif,modify-time,create-time" -on-conflict skip -recursive
