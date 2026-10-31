// Siri Remote Indigo wire phases follow Meta idb (MIT); see LICENSE-idb.
#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <dlfcn.h>
#import <unistd.h>
#import <math.h>
#import <malloc/malloc.h>
@interface NSObject (PrivateSim)
+ (id)sharedServiceContextForDeveloperDir:(NSString *)dir error:(NSError **)error;
- (id)defaultDeviceSetWithError:(NSError **)error;
- (NSArray *)availableDevices;
- (NSUUID *)UDID;
- (id)initWithDevice:(id)device error:(NSError **)error;
- (void)sendWithMessage:(void *)msg freeWhenDone:(BOOL)free completionQueue:(dispatch_queue_t)queue completion:(void (^)(NSError *))completion;
@end
int main(int argc, char **argv) { @autoreleasepool { @try {
 if(argc!=8) { fprintf(stderr,"usage: tvpan developer-dir udid x1 y1 x2 y2 duration-ms\n"); return 2; }
 double coords[5];
 for(int n=0;n<5;n++){ char *end;coords[n]=strtod(argv[n+3],&end);if(end==argv[n+3] || *end || !isfinite(coords[n]) || coords[n]<0 || coords[n]>(n==4?10000:1))return 2; }
 if(coords[4]<1)return 2;
 NSString *dev=@(argv[1]), *udid=@(argv[2]);
 NSArray *paths=@[@"/Library/Developer/PrivateFrameworks/CoreSimulator.framework/CoreSimulator",[[dev stringByDeletingLastPathComponent] stringByAppendingString:@"/SharedFrameworks/SimulatorKit.framework/SimulatorKit"]];
 for(NSString *path in paths) if(!dlopen(path.UTF8String,RTLD_NOW|RTLD_GLOBAL)){fprintf(stderr,"%s\n",dlerror());return 1;}
 NSError *error=nil;
 id context=[NSClassFromString(@"SimServiceContext") sharedServiceContextForDeveloperDir:dev error:&error];
 id set=[context defaultDeviceSetWithError:&error]; id device=nil;
 for(id candidate in [set availableDevices]) if([[[candidate UDID] UUIDString] isEqualToString:udid]) device=candidate;
 if(!device){fprintf(stderr,"device not found\n");return 1;}
 id client=[[NSClassFromString(@"SimulatorKit.SimDeviceLegacyHIDClient") alloc] initWithDevice:device error:&error];
 if(!client){fprintf(stderr,"client: %s\n",error.description.UTF8String);return 1;}
 void *(*build)(CGPoint,uint32_t)=dlsym(RTLD_DEFAULT,"IndigoHIDMessageForTrackpadMoveEvent");
 if(!build) return 1;
 dispatch_group_t group=dispatch_group_create(); dispatch_queue_t queue=dispatch_queue_create("tvpan",0);
 __block BOOL failed=NO;
 for(int i=0;i<=13;i++){
  double t=MIN(i,12)/12.;void *message=build(CGPointMake(coords[0]+(coords[2]-coords[0])*t,coords[1]+(coords[3]-coords[1])*t),0x16);
  if(!message)return 1;
  // SimulatorKit's trackpad builder must provide both wire payloads.
  if(malloc_size(message)<0x10c){free(message);fprintf(stderr,"unsupported trackpad message layout\n");return 1;}
  uint8_t *p=message;
  if(i==0){*(uint32_t*)(p+0x38)=0x23;*(uint32_t*)(p+0xd8)=3;}
  if(i==13){*(uint32_t*)(p+0x38)=0x21;*(uint32_t*)(p+0x64)=0;*(uint32_t*)(p+0x68)=0;*(uint32_t*)(p+0xd8)=1;*(uint32_t*)(p+0x104)=0;*(uint32_t*)(p+0x108)=0;}
  dispatch_group_enter(group);
  [client sendWithMessage:message freeWhenDone:YES completionQueue:queue completion:^(NSError *e){if(e){failed=YES;fprintf(stderr,"send: %s\n",e.description.UTF8String);}dispatch_group_leave(group);}];
  if(i<13)usleep((useconds_t)(coords[4]*1000/13));
 }
 if(dispatch_group_wait(group,dispatch_time(DISPATCH_TIME_NOW,5*NSEC_PER_SEC))) return 1;
 return failed?1:0;
 }@catch(NSException *e){fprintf(stderr,"%s\n",e.description.UTF8String);return 1;}}}
