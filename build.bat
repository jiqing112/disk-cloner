@echo off
chcp 65001 >nul
title Disk Cloner - Go Builder
setlocal enabledelayedexpansion

echo ============================================
echo   Disk Cloner - Go Build
echo ============================================
echo.

where go >nul 2>&1
if %errorlevel% neq 0 (
    echo [ERROR] Go is not installed!
    echo Please install Go from: https://go.dev/dl/
    pause
    exit /b 1
)

for /f "tokens=3" %%i in ('go version') do echo Go version: %%i

cd /d "%~dp0"

echo.
echo [1/2] Downloading dependencies...
go mod tidy
if %errorlevel% neq 0 (
    echo [FAILED] go mod tidy failed
    pause
    exit /b 1
)
echo [OK] Dependencies ready.

echo.
echo [2/2] Building...

echo   - Building for Linux amd64 (Alpine)...
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags="-s -w" -o disk-cloner-linux-amd64 .
if %errorlevel% neq 0 (
    echo     [FAILED]
) else (
    echo     [OK] disk-cloner-linux-amd64
)

echo   - Building for Windows amd64...
set GOOS=windows
set GOARCH=amd64
go build -ldflags="-s -w" -o disk-cloner-windows-amd64.exe .
if %errorlevel% neq 0 (
    echo     [FAILED]
) else (
    echo     [OK] disk-cloner-windows-amd64.exe
)

echo.
echo ============================================
echo   BUILD COMPLETE
echo ============================================
echo.
echo Deploy to Alpine Linux client:
echo   scp disk-cloner-linux-amd64 root@CLIENT:/usr/local/bin/disk-cloner
echo   ssh root@CLIENT
echo     chmod +x /usr/local/bin/disk-cloner
echo     disk-cloner
echo.
echo Use on Windows:
echo   disk-cloner-windows-amd64.exe

pause
exit /b 0
