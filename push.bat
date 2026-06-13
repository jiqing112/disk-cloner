@echo off
chcp 65001 >nul
title Disk Cloner - Quick Push
setlocal enabledelayedexpansion

cd /d "%~dp0"

echo.
echo ========================================
echo   Disk Cloner - Push to GitHub
echo ========================================
echo.

git add -A
if %errorlevel% neq 0 (
    echo [ERR] git add failed
    pause
    exit /b 1
)

git status

echo.
set /p msg="Commit message: "

if "%msg%"=="" (
    echo [ERR] Commit message required
    pause
    exit /b 1
)

git commit -m "%msg%"
if %errorlevel% neq 0 (
    echo [ERR] git commit failed
    pause
    exit /b 1
)

git push
if %errorlevel% neq 0 (
    echo [ERR] git push failed
    pause
    exit /b 1
)

echo.
echo [OK] Pushed to GitHub!
echo.
pause
