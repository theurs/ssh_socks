@echo off
setlocal enabledelayedexpansion

REM Read version from VERSION file
for /f "tokens=*" %%i in (VERSION) do set VERSION=%%i

echo Building release version !VERSION!

REM Create build folders
if not exist "dist" mkdir "dist"
if not exist "dist\windows-amd64" mkdir "dist\windows-amd64"

REM Build binary
echo Building ssh_proxy...
go build -ldflags "-H=windowsgui -s -w -X main.Version=!VERSION!" -o dist\windows-amd64\ssh_proxy.exe .
if !errorlevel! neq 0 (
    echo Error building ssh_proxy
    exit /b !errorlevel!
)

REM Copy version file to build folder
copy VERSION dist\windows-amd64\

REM Archive
echo Creating archive with maximum compression...
cd dist\windows-amd64
7z a -tzip -mx=9 "..\ssh_proxy-v!VERSION!-windows-amd64.zip" *.*
if !errorlevel! neq 0 (
    echo Error archiving
    exit /b !errorlevel!
)
cd ..\..

REM Create GitHub release
echo Creating GitHub release...

REM Check if a release with the same tag already exists and delete it
gh release view "v!VERSION!" >nul 2>&1
if !errorlevel! equ 0 (
    echo Found existing release, deleting it...
    gh release delete "v!VERSION!" --yes 2>nul
)

REM Create the new release
gh release create "v!VERSION!" "dist\ssh_proxy-v!VERSION!-windows-amd64.zip" --title "v!VERSION!" --notes "Release version !VERSION!"

if !errorlevel! equ 0 (
    echo Release v!VERSION! successfully created!
) else (
    echo Error creating release
    exit /b !errorlevel!
)

echo Build and publish completed!

REM Cleanup dist folder
echo Cleaning up dist folder...
if exist dist\*.zip del /q dist\*.zip
del /q dist\windows-amd64\*
rmdir /s /q dist\windows-amd64
